package scdl

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/mp4"
)

// fmp4Fixture builds a fragmented AAC stream like SoundCloud serves: an init
// segment (carrying its own udta, as SoundCloud's does) and two media segments
// with two samples each.
func fmp4Fixture(t *testing.T) (initSegment []byte, segments [][]byte) {
	t.Helper()

	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(44100, "audio", "und")
	if err := trak.SetAACDescriptor(aac.AAClc, 44100); err != nil {
		t.Fatal(err)
	}
	encoderUdta := &mp4.UdtaBox{}
	encoderUdta.AddChild(mp4.NewGenericContainerBox("free"))
	init.Moov.AddChild(encoderUdta)

	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	initSegment = buf.Bytes()

	var decodeTime uint64
	for seq := range 2 {
		frag, err := mp4.CreateFragment(uint32(seq+1), trak.Tkhd.TrackID) //nolint:gosec // tiny test loop
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			data := []byte("aac-frame")
			frag.AddFullSample(mp4.FullSample{
				Sample:     mp4.NewSample(mp4.SyncSampleFlags, 1024, uint32(len(data)), 0),
				DecodeTime: decodeTime,
				Data:       data,
			})
			decodeTime += 1024
		}
		var seg bytes.Buffer
		if err := frag.Encode(&seg); err != nil {
			t.Fatal(err)
		}
		segments = append(segments, seg.Bytes())
	}

	return initSegment, segments
}

func aacClient(t *testing.T, playlist string, initSegment []byte, segments [][]byte) *Client {
	t.Helper()
	return &Client{
		clientID: "test",
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					u := req.URL.String()
					body := []byte(nil)
					switch {
					case strings.Contains(u, "/stream/hls"):
						body = []byte(`{"url": "http://mock/aac/playlist.m3u8"}`)
					case u == "http://mock/aac/playlist.m3u8":
						body = []byte(playlist)
					case u == "http://mock/aac/init.mp4" && initSegment != nil:
						body = initSegment
					case u == "http://mock/aac/data000.m4s":
						body = segments[0]
					case u == "http://mock/aac/data001.m4s":
						body = segments[1]
					case u == "http://mock/artwork-t500x500.png":
						body = []byte("fake png")
					default:
						return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("Not Found: " + u))}, nil
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
				},
			},
		},
	}
}

const aacPlaylist = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:10
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MAP:URI="init.mp4"
#EXTINF:0.046,
data000.m4s
#EXTINF:0.046,
data001.m4s
#EXT-X-ENDLIST`

func TestDownload_AAC(t *testing.T) {
	initSegment, segments := fmp4Fixture(t)
	client := aacClient(t, aacPlaylist, initSegment, segments)

	track := &Track{
		ID:          1,
		Title:       "Title",
		Artist:      "Artist",
		Album:       "Album",
		Year:        "2026",
		Genre:       "Genre",
		Description: "Description",
		ArtworkURL:  "http://mock/artwork-large.png",
		HLSURL:      "https://api-v2.soundcloud.com/media/soundcloud:tracks:1/token/stream/hls",
		Format:      FormatAAC,
	}

	var progressCalls int
	outPath, err := client.Download(context.Background(), track, t.TempDir(), func(int, int) { progressCalls++ })
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if !strings.HasSuffix(outPath, "Artist - Title.m4a") {
		t.Errorf("unexpected output path %q", outPath)
	}
	if progressCalls != 2 {
		t.Errorf("expected 2 progress calls, got %d", progressCalls)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}

	if f.IsFragmented() || f.Moov.Mvex != nil {
		t.Error("output is still fragmented")
	}
	if got := f.Moov.Trak.Mdia.Minf.Stbl.Stsz.SampleNumber; got != 4 {
		t.Errorf("got %d samples, want 4", got)
	}
	if got := f.Moov.Mvhd.Duration; got == 0 {
		t.Error("movie duration not set")
	}

	var udtas []*mp4.UdtaBox
	for _, child := range f.Moov.Children {
		if u, ok := child.(*mp4.UdtaBox); ok {
			udtas = append(udtas, u)
		}
	}
	if len(udtas) != 1 {
		t.Fatalf("got %d udta boxes, want 1 (init segment udta should be replaced)", len(udtas))
	}

	tags := map[string]*mp4.DataBox{}
	for _, item := range ilstItems(t, udtas[0]) {
		for _, child := range item.(*mp4.GenericContainerBox).Children {
			if d, ok := child.(*mp4.DataBox); ok {
				tags[item.Type()] = d
			}
		}
	}

	wantText := map[string]string{
		"\xa9nam": "Title",
		"\xa9ART": "Artist",
		"\xa9alb": "Album",
		"\xa9day": "2026",
		"\xa9gen": "Genre",
		"\xa9cmt": "Description",
	}
	for typ, want := range wantText {
		d, ok := tags[typ]
		if !ok {
			t.Errorf("missing tag %q", typ)
			continue
		}
		if got := string(d.Data); got != want {
			t.Errorf("tag %q = %q, want %q", typ, got, want)
		}
	}

	covr, ok := tags["covr"]
	if !ok {
		t.Fatal("missing cover art")
	}
	if covr.DataType != mp4.DataTypePNG || string(covr.Data) != "fake png" {
		t.Errorf("unexpected cover: type %d, data %q", covr.DataType, covr.Data)
	}
}

func ilstItems(t *testing.T, udta *mp4.UdtaBox) []mp4.Box {
	t.Helper()
	for _, child := range udta.Children {
		meta, ok := child.(*mp4.MetaBox)
		if !ok {
			continue
		}
		for _, mc := range meta.Children {
			if ilst, ok := mc.(*mp4.IlstBox); ok {
				return ilst.Children
			}
		}
	}
	t.Fatal("no ilst in udta")
	return nil
}

func TestDownload_AAC_MinimalMetadata(t *testing.T) {
	initSegment, segments := fmp4Fixture(t)
	client := aacClient(t, aacPlaylist, initSegment, segments)

	track := &Track{
		Title:      "Title",
		Artist:     "Artist",
		ArtworkURL: "http://mock/missing-large.jpg",
		HLSURL:     "https://api-v2.soundcloud.com/media/soundcloud:tracks:1/token/stream/hls",
		Format:     FormatAAC,
	}

	outPath, err := client.Download(context.Background(), track, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	for _, child := range f.Moov.Children {
		udta, ok := child.(*mp4.UdtaBox)
		if !ok {
			continue
		}
		var types []string
		for _, item := range ilstItems(t, udta) {
			types = append(types, item.Type())
		}
		if got := strings.Join(types, ","); got != "\xa9nam,\xa9ART" {
			t.Errorf("got tags %q, want only title and artist", got)
		}
	}
}

func TestDownload_AAC_Errors(t *testing.T) {
	initSegment, segments := fmp4Fixture(t)
	track := &Track{
		Title:  "Title",
		Artist: "Artist",
		HLSURL: "https://api-v2.soundcloud.com/media/soundcloud:tracks:1/token/stream/hls",
		Format: FormatAAC,
	}

	tests := []struct {
		name     string
		playlist string
		init     []byte
		outDir   string
		wantErr  string
	}{
		{
			name:     "NoInitSegment",
			playlist: strings.Replace(aacPlaylist, "#EXT-X-MAP:URI=\"init.mp4\"\n", "", 1),
			init:     initSegment,
			wantErr:  "no init segment in playlist",
		},
		{
			name:     "InitFetchFail",
			playlist: aacPlaylist,
			init:     nil,
			wantErr:  "download init segment",
		},
		{
			name:     "InvalidMP4",
			playlist: aacPlaylist,
			init:     []byte("not an mp4"),
			wantErr:  "decode MP4",
		},
		{
			name:     "WriteFail",
			playlist: aacPlaylist,
			init:     initSegment,
			outDir:   "/nonexistent/dir",
			wantErr:  "write output file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := aacClient(t, tt.playlist, tt.init, segments)
			outDir := tt.outDir
			if outDir == "" {
				outDir = t.TempDir()
			}
			_, err := client.Download(context.Background(), track, outDir, nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got error %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestWriteM4A_NoMoov(t *testing.T) {
	_, segments := fmp4Fixture(t)
	client := &Client{}
	err := client.writeM4A(context.Background(), t.TempDir()+"/out.m4a", nil, segments, &Track{})
	if err == nil || !strings.Contains(err.Error(), "no moov box") {
		t.Errorf("got error %v, want missing moov", err)
	}
}

func TestWriteM4A_DefragmentFail(t *testing.T) {
	initSegment, _ := fmp4Fixture(t)
	client := &Client{}
	// An init segment without fragments leaves the track without samples.
	err := client.writeM4A(context.Background(), t.TempDir()+"/out.m4a", initSegment, nil, &Track{})
	if err == nil || !strings.Contains(err.Error(), "defragment MP4") {
		t.Errorf("got error %v, want defragment failure", err)
	}
}
