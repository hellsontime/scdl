package scdl

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bogem/id3v2/v2"
	"github.com/grafov/m3u8"
	"golang.org/x/sync/errgroup"
)

const maxConcurrentSegments = 20

// Download fetches the track audio via HLS and saves it as an MP3 with ID3 tags
// or, for AAC streams, as an M4A with iTunes-style tags.
// Returns the output file path.
func (c *Client) Download(ctx context.Context, track *Track, outputDir string, progress func(downloaded, total int)) (outPath string, err error) {
	m3u8URL, err := c.GetStreamURL(ctx, track)
	if err != nil {
		return "", err
	}

	mpl, err := c.parseM3U8(ctx, m3u8URL)
	if err != nil {
		return "", fmt.Errorf("parse M3U8: %w", err)
	}

	segCount := mpl.Count()
	if segCount == 0 {
		return "", fmt.Errorf("no segments in playlist")
	}
	if segCount > math.MaxInt {
		return "", fmt.Errorf("playlist too large")
	}

	// Fragmented MP4 streams carry their codec setup in a separate init segment.
	var initSegment []byte
	if track.Format == FormatAAC {
		if mpl.Map == nil || mpl.Map.URI == "" {
			return "", fmt.Errorf("no init segment in playlist")
		}
		initSegment, err = c.get(ctx, mpl.Map.URI)
		if err != nil {
			return "", fmt.Errorf("download init segment: %w", err)
		}
	}

	segments, err := c.downloadSegments(ctx, mpl, progress)
	if err != nil {
		return "", err
	}

	filename := sanitizeFilename(track.Artist+" - "+track.Title) + track.Format.Ext()
	outPath = filepath.Clean(filepath.Join(outputDir, filename))

	if track.Format == FormatAAC {
		err = c.writeM4A(ctx, outPath, initSegment, segments, track)
	} else {
		err = c.writeMP3(ctx, outPath, segments, track)
	}
	if err != nil {
		return "", err
	}

	return outPath, nil
}

// downloadSegments fetches and decrypts all media segments of the playlist
// concurrently, returning them in playlist order.
func (c *Client) downloadSegments(ctx context.Context, mpl *m3u8.MediaPlaylist, progress func(downloaded, total int)) ([][]byte, error) {
	count := int(mpl.Count()) //nolint:gosec // bounds checked in Download

	segments := make([][]byte, count)
	var keyCache sync.Map

	g, groupCtx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentSegments)

	var progressMu sync.Mutex
	downloaded := 0

	for i := range count {
		seg := mpl.Segments[i]
		globalKey := mpl.Key

		g.Go(func() error {
			data, err := c.get(groupCtx, seg.URI)
			if err != nil {
				return fmt.Errorf("download segment %d: %w", i, err)
			}

			data, err = c.decryptSegment(groupCtx, data, seg, globalKey, i, &keyCache)
			if err != nil {
				return fmt.Errorf("decrypt segment %d: %w", i, err)
			}

			segments[i] = data

			if progress != nil {
				progressMu.Lock()
				downloaded++
				progress(downloaded, count)
				progressMu.Unlock()
			}

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return segments, nil
}

// writeMP3 concatenates the MPEG audio segments into outPath and tags it with ID3.
func (c *Client) writeMP3(ctx context.Context, outPath string, segments [][]byte, track *Track) error {
	f, err := os.Create(outPath) //nolint:gosec // user controls output path
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}

	for _, seg := range segments {
		if _, err := f.Write(seg); err != nil {
			_ = f.Close()
			return fmt.Errorf("write segment: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close output file: %w", err)
	}

	if err := c.embedMetadata(ctx, outPath, track); err != nil {
		return fmt.Errorf("embed metadata: %w", err)
	}

	return nil
}

func (c *Client) parseM3U8(ctx context.Context, m3u8URL string) (*m3u8.MediaPlaylist, error) {
	data, err := c.get(ctx, m3u8URL)
	if err != nil {
		return nil, err
	}

	playlist, listType, err := m3u8.DecodeFrom(bytes.NewReader(data), true)
	if err != nil {
		return nil, err
	}

	if listType != m3u8.MEDIA {
		return nil, fmt.Errorf("unsupported playlist type (expected media)")
	}

	mpl := playlist.(*m3u8.MediaPlaylist)
	base, _ := url.Parse(m3u8URL)

	if mpl.Key != nil && mpl.Key.URI != "" {
		mpl.Key.URI, err = resolveURI(base, mpl.Key.URI)
		if err != nil {
			return nil, err
		}
	}

	if mpl.Map != nil && mpl.Map.URI != "" {
		mpl.Map.URI, err = resolveURI(base, mpl.Map.URI)
		if err != nil {
			return nil, err
		}
	}

	for i := range int(mpl.Count()) { //nolint:gosec // bounds checked in Download
		seg := mpl.Segments[i]
		seg.URI, err = resolveURI(base, seg.URI)
		if err != nil {
			return nil, err
		}
		if seg.Key != nil && seg.Key.URI != "" {
			seg.Key.URI, err = resolveURI(base, seg.Key.URI)
			if err != nil {
				return nil, err
			}
		}
	}

	return mpl, nil
}

func (c *Client) decryptSegment(ctx context.Context, data []byte, seg *m3u8.MediaSegment, globalKey *m3u8.Key, index int, keyCache *sync.Map) ([]byte, error) {
	var keyURL, ivStr string
	if seg.Key != nil && seg.Key.URI != "" {
		keyURL = seg.Key.URI
		ivStr = seg.Key.IV
	} else if globalKey != nil && globalKey.URI != "" {
		keyURL = globalKey.URI
		ivStr = globalKey.IV
	}

	if keyURL == "" {
		return data, nil
	}

	key, err := c.fetchKey(ctx, keyURL, keyCache)
	if err != nil {
		return nil, fmt.Errorf("fetch key: %w", err)
	}

	var iv []byte
	if ivStr != "" {
		iv, err = hex.DecodeString(strings.TrimPrefix(ivStr, "0x"))
		if err != nil {
			return nil, fmt.Errorf("decode IV: %w", err)
		}
	} else {
		iv = make([]byte, 16)
		binary.BigEndian.PutUint32(iv[12:], uint32(index)) //nolint:gosec // index bounded by playlist segment count
	}

	return decryptAES128CBC(data, key, iv)
}

func (c *Client) fetchKey(ctx context.Context, keyURL string, cache *sync.Map) ([]byte, error) {
	if cached, ok := cache.Load(keyURL); ok {
		return cached.([]byte), nil
	}

	key, err := c.get(ctx, keyURL)
	if err != nil {
		return nil, err
	}

	actual, _ := cache.LoadOrStore(keyURL, key)
	return actual.([]byte), nil
}

func decryptAES128CBC(data, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext is not a multiple of block size")
	}

	cbc := cipher.NewCBCDecrypter(block, iv)
	cbc.CryptBlocks(data, data)

	// PKCS#7 unpadding
	if len(data) == 0 {
		return data, nil
	}
	padding := int(data[len(data)-1])
	if padding > aes.BlockSize || padding == 0 {
		return data, nil
	}
	for i := range padding {
		if data[len(data)-1-i] != byte(padding) {
			return data, nil
		}
	}
	return data[:len(data)-padding], nil
}

func (c *Client) embedMetadata(ctx context.Context, filePath string, track *Track) (err error) {
	tag, err := id3v2.Open(filePath, id3v2.Options{Parse: true})
	if err != nil {
		return fmt.Errorf("open for tagging: %w", err)
	}
	defer func() {
		if closeErr := tag.Close(); closeErr != nil {
			if err == nil {
				err = fmt.Errorf("close tag: %w", closeErr)
			}
		}
	}()

	tag.SetTitle(track.Title)
	tag.SetArtist(track.Artist)
	tag.SetGenre(track.Genre)

	if track.Album != "" {
		tag.SetAlbum(track.Album)
	}

	if track.Year != "" {
		tag.SetYear(track.Year)
	}

	if track.Description != "" {
		tag.AddCommentFrame(id3v2.CommentFrame{
			Encoding:    id3v2.EncodingUTF8,
			Language:    "eng",
			Description: "",
			Text:        track.Description,
		})
	}

	if image, mimeType := c.fetchImage(ctx, track.ArtworkURL); image != nil {
		tag.AddAttachedPicture(id3v2.PictureFrame{
			Encoding:    id3v2.EncodingUTF8,
			MimeType:    mimeType,
			PictureType: id3v2.PTFrontCover,
			Description: "Front cover",
			Picture:     image,
		})
	}

	if image, mimeType := c.fetchImage(ctx, track.ArtistAvatarURL); image != nil {
		tag.AddAttachedPicture(id3v2.PictureFrame{
			Encoding:    id3v2.EncodingUTF8,
			MimeType:    mimeType,
			PictureType: id3v2.PTArtistPerformer,
			Description: "Artist",
			Picture:     image,
		})
	}

	return tag.Save()
}

// fetchImage downloads a SoundCloud image, preferring its 500x500 variant and
// falling back to the original URL. It returns nil if the URL is empty or
// neither variant can be fetched.
func (c *Client) fetchImage(ctx context.Context, imageURL string) (image []byte, mimeType string) {
	if imageURL == "" {
		return nil, ""
	}

	hiResURL := strings.Replace(imageURL, "-large.", "-t500x500.", 1)
	image, err := c.get(ctx, hiResURL)
	finalURL := hiResURL
	if (err != nil || len(image) == 0) && hiResURL != imageURL {
		// Fallback to original URL if high-res fails and we changed it
		image, err = c.get(ctx, imageURL)
		finalURL = imageURL
	}

	if err != nil || len(image) == 0 {
		return nil, ""
	}
	return image, imageMimeType(finalURL)
}

func resolveURI(base *url.URL, uri string) (string, error) {
	if strings.HasPrefix(uri, "http") {
		return uri, nil
	}
	ref, err := base.Parse(uri)
	if err != nil {
		return "", err
	}
	return ref.String(), nil
}

const (
	mimeJPEG = "image/jpeg"
	mimePNG  = "image/png"
)

var imageExtMimeTypes = map[string]string{
	".jpg":  mimeJPEG,
	".jpeg": mimeJPEG,
	".png":  mimePNG,
	".webp": "image/webp",
}

func imageMimeType(url string) string {
	for ext, mime := range imageExtMimeTypes {
		if strings.HasSuffix(url, ext) {
			return mime
		}
	}
	return mimeJPEG
}

func sanitizeFilename(name string) string {
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
	)
	name = replacer.Replace(name)
	name = strings.ReplaceAll(name, "\x00", "")
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "untitled"
	}
	return name
}
