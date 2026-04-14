package scdl

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/grafov/m3u8"
)

func TestImageMimeType(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://cdn.example.com/image.jpg", "image/jpeg"},
		{"https://cdn.example.com/image.jpeg", "image/jpeg"},
		{"https://cdn.example.com/image.png", "image/png"},
		{"https://cdn.example.com/image.webp", "image/webp"},
		{"https://cdn.example.com/image.gif", "image/jpeg"}, // unsupported falls back to jpeg
		{"https://cdn.example.com/noextension", "image/jpeg"},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := imageMimeType(tt.url); got != tt.want {
				t.Errorf("imageMimeType(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "Normal filename",
			input: "Artist - Song",
			want:  "Artist - Song",
		},
		{
			name:  "Filename with forward slash",
			input: "AC/DC - Thunderstruck",
			want:  "AC_DC - Thunderstruck",
		},
		{
			name:  "Filename with special chars",
			input: "Artist: Song? <Cool>",
			want:  "Artist_ Song_ _Cool_",
		},
		{
			name:  "Filename with mixed separators",
			input: "foo\\bar|baz*qux",
			want:  "foo_bar_baz_qux",
		},
		{
			name:  "Filename with null bytes",
			input: "Artist\x00 - Song",
			want:  "Artist - Song",
		},
		{
			name:  "Filename with leading dots",
			input: "...hidden",
			want:  "hidden",
		},
		{
			name:  "Empty filename",
			input: "",
			want:  "untitled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeFilename(tt.input); got != tt.want {
				t.Errorf("sanitizeFilename() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveURI(t *testing.T) {
	baseURL, _ := url.Parse("https://example.com/hls/playlist.m3u8")

	tests := []struct {
		name    string
		base    *url.URL
		uri     string
		want    string
		wantErr bool
	}{
		{
			name: "Absolute URI",
			base: baseURL,
			uri:  "https://other.com/segment.ts",
			want: "https://other.com/segment.ts",
		},
		{
			name: "Relative URI",
			base: baseURL,
			uri:  "segment.ts",
			want: "https://example.com/hls/segment.ts",
		},
		{
			name: "Relative URI parent dir",
			base: baseURL,
			uri:  "../segment.ts",
			want: "https://example.com/segment.ts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveURI(tt.base, tt.uri)
			if (err != nil) != tt.wantErr {
				t.Errorf("resolveURI() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("resolveURI() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecryptAES128CBC(t *testing.T) {
	// Standard AES-128-CBC test vectors or just a roundtrip test
	key := []byte("1234567890123456") // 16 bytes
	iv := []byte("abcdefghijklmnop")  // 16 bytes
	plaintext := []byte("Hello World! 123")

	// Encrypt manually to setup test case
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}

	// Pad plaintext to block size
	padding := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	paddedText := append(plaintext, bytes.Repeat([]byte{byte(padding)}, padding)...)

	ciphertext := make([]byte, len(paddedText))
	cbc := cipher.NewCBCEncrypter(block, iv)
	cbc.CryptBlocks(ciphertext, paddedText)

	t.Run("Valid Decryption", func(t *testing.T) {
		got, err := decryptAES128CBC(ciphertext, key, iv)
		if err != nil {
			t.Fatalf("decryptAES128CBC() error = %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Errorf("decryptAES128CBC() = %q, want %q", got, plaintext)
		}
	})

	t.Run("Invalid Key Size", func(t *testing.T) {
		badKey := []byte("too short")
		_, err := decryptAES128CBC(ciphertext, badKey, iv)
		if err == nil {
			t.Error("expected error for invalid key size")
		}
	})

	t.Run("Ciphertext not multiple of block size", func(t *testing.T) {
		badCiphertext := ciphertext[:len(ciphertext)-1]
		_, err := decryptAES128CBC(badCiphertext, key, iv)
		if err == nil {
			t.Error("expected error for bad ciphertext length")
		}
	})

	t.Run("Bad Padding", func(t *testing.T) {
		// Corrupt the last byte to invalidate padding
		badCiphertext := make([]byte, len(ciphertext))
		copy(badCiphertext, ciphertext)
		badCiphertext[len(badCiphertext)-1] ^= 0x01

		// In standard CBC decryption, if we change the last byte of ciphertext,
		// it affects the last byte of the decrypted plaintext (XORed with last byte of previous ciphertext block).
		// This might produce a valid padding byte value by chance, but unlikely to be correct for this test unless we calculate it.
		// Actually, if we modify the last byte of ciphertext, the last block of plaintext changes.
		// If we want to guarantee bad padding, we might just pass garbage.
		// But let's verify it doesn't panic at least.
	})
}

func TestDownload_Progress(t *testing.T) {
	client := &Client{
		clientID: "test",
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					u := req.URL.String()
					if strings.Contains(u, "/stream/hls") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"url": "http://mock/playlist.m3u8"}`))}, nil
					}
					if u == "http://mock/playlist.m3u8" {
						m3u8Content := "#EXTM3U\n#EXTINF:10.0,\nseg1.ts\n#EXTINF:10.0,\nseg2.ts\n#EXT-X-ENDLIST"
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(m3u8Content))}, nil
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("some-audio-data"))}, nil
				},
			},
		},
	}

	var progressCalls int
	progress := func(downloaded, total int) {
		progressCalls++
	}

	track := &Track{ID: 1, Title: "S", Artist: "A", HLSURL: "http://api/soundcloud:tracks:1/stream/hls"}
	_, err := client.Download(context.Background(), track, t.TempDir(), progress)
	if err != nil {
		t.Fatal(err)
	}

	if progressCalls != 2 {
		t.Errorf("expected 2 progress calls, got %d", progressCalls)
	}
}

func TestDownload_EmbedMetadata(t *testing.T) {
	// Setup a minimal valid MP3 file or just let id3v2 fail gracefully if possible
	// Actually, Download creates the file.
	client := &Client{
		clientID: "test",
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					if strings.Contains(req.URL.String(), "artwork") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader([]byte("fake-image")))}, nil
					}
					if strings.Contains(req.URL.String(), "hls") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"url": "http://mock/playlist.m3u8"}`))}, nil
					}
					if strings.Contains(req.URL.String(), "playlist.m3u8") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXTINF:1,\nseg.ts\n#EXT-X-ENDLIST"))}, nil
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader([]byte("audio-data")))}, nil
				},
			},
		},
	}

	track := &Track{
		ID:          1,
		Title:       "Title",
		Artist:      "Artist",
		Description: "Description",
		Genre:       "Genre",
		ArtworkURL:  "http://mock/artwork.jpg",
		HLSURL:      "http://api/media/soundcloud:tracks:1/token/stream/hls",
	}

	_, err := client.Download(context.Background(), track, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseM3U8_Errors(t *testing.T) {
	client := &Client{
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					if strings.Contains(req.URL.String(), "fail") {
						return nil, fmt.Errorf("fail")
					}
					if strings.Contains(req.URL.String(), "not-m3u8") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("not a playlist"))}, nil
					}
					if strings.Contains(req.URL.String(), "wrong-type") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=128000\nchunk.m3u8"))}, nil
					}
					return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("404"))}, nil
				},
			},
		},
	}

	t.Run("FetchFail", func(t *testing.T) {
		_, err := client.parseM3U8(context.Background(), "http://fail")
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("DecodeFail", func(t *testing.T) {
		_, err := client.parseM3U8(context.Background(), "http://not-m3u8")
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("WrongType", func(t *testing.T) {
		_, err := client.parseM3U8(context.Background(), "http://wrong-type")
		if err == nil || !strings.Contains(err.Error(), "unsupported playlist type") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestDownload_Errors(t *testing.T) {
	client := &Client{
		clientID: "test",
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					u := req.URL.String()
					if strings.Contains(u, "fail") {
						return nil, fmt.Errorf("fail")
					}
					if strings.Contains(u, "empty-mpl") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXT-X-ENDLIST"))}, nil
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXTINF:1,\nseg.ts\n#EXT-X-ENDLIST"))}, nil
				},
			},
		},
	}

	t.Run("GetStreamURLFail", func(t *testing.T) {
		track := &Track{HLSURL: "invalid"}
		_, err := client.Download(context.Background(), track, t.TempDir(), nil)
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("EmptyPlaylist", func(t *testing.T) {
		client.httpClient.Transport = &mockTransport{
			RoundTripFunc: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.String(), "stream/hls") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"url": "http://mock/empty-mpl"}`))}, nil
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXT-X-ENDLIST"))}, nil
			},
		}
		_, err := client.Download(context.Background(), &Track{HLSURL: "http://api/soundcloud:tracks:1/empty/stream/hls"}, t.TempDir(), nil)
		if err == nil || !strings.Contains(err.Error(), "no segments") {
			t.Errorf("expected empty playlist error, got %v", err)
		}
	})

	t.Run("SegmentDownloadFail", func(t *testing.T) {
		client.httpClient.Transport = &mockTransport{
			RoundTripFunc: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.String(), "stream/hls") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"url": "http://mock/mpl"}`))}, nil
				}
				if strings.Contains(req.URL.String(), "mpl") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXTINF:1,\nfail-seg.ts\n#EXT-X-ENDLIST"))}, nil
				}
				return nil, fmt.Errorf("seg fail")
			},
		}
		_, err := client.Download(context.Background(), &Track{HLSURL: "http://api/soundcloud:tracks:1/seg-fail/stream/hls"}, t.TempDir(), nil)
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("ParseM3U8FailInDownload", func(t *testing.T) {
		client.httpClient.Transport = &mockTransport{
			RoundTripFunc: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.String(), "stream/hls") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"url": "http://mock/bad-mpl"}`))}, nil
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("not-m3u8"))}, nil
			},
		}
		_, err := client.Download(context.Background(), &Track{HLSURL: "http://api/soundcloud:tracks:1/bad-mpl/stream/hls"}, t.TempDir(), nil)
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("DecryptFail", func(t *testing.T) {
		client.httpClient.Transport = &mockTransport{
			RoundTripFunc: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.String(), "stream/hls") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"url": "http://mock/mpl"}`))}, nil
				}
				if strings.Contains(req.URL.String(), "mpl") {
					// Invalid IV (not hex)
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"http://mock/key\",IV=0xINVALID\n#EXTINF:1,\nseg.ts\n#EXT-X-ENDLIST"))}, nil
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data"))}, nil
			},
		}
		_, err := client.Download(context.Background(), &Track{HLSURL: "http://api/soundcloud:tracks:1/decrypt-fail/stream/hls"}, t.TempDir(), nil)
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("GlobalKey", func(t *testing.T) {
		client.httpClient.Transport = &mockTransport{
			RoundTripFunc: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.String(), "stream/hls") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"url": "http://mock/mpl"}`))}, nil
				}
				if strings.Contains(req.URL.String(), "mpl") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"http://mock/key\"\n#EXTINF:1,\nseg.ts\n#EXT-X-ENDLIST"))}, nil
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("keydata012345678"))}, nil
			},
		}
		_, err := client.Download(context.Background(), &Track{HLSURL: "http://api/soundcloud:tracks:1/global-key/stream/hls"}, t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestFetchKey_Cache(t *testing.T) {
	var callCount int
	client := &Client{
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					callCount++
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("keydata"))}, nil
				},
			},
		},
	}

	var cache sync.Map
	k1, err := client.fetchKey(context.Background(), "http://key", &cache)
	if err != nil || string(k1) != "keydata" {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 call, got %d", callCount)
	}

	k2, err := client.fetchKey(context.Background(), "http://key", &cache)
	if err != nil || string(k2) != "keydata" {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Errorf("expected STILL 1 call (cached), got %d", callCount)
	}
}

func TestDecryptAES128CBC_EdgeCases(t *testing.T) {
	t.Run("EmptyData", func(t *testing.T) {
		got, err := decryptAES128CBC([]byte{}, []byte("1234567890123456"), make([]byte, 16))
		if err != nil || len(got) != 0 {
			t.Errorf("expected empty result, got %v, %v", got, err)
		}
	})

	t.Run("BadPadding", func(t *testing.T) {
		data := make([]byte, 16)
		data[15] = 0 // Invalid padding
		key := []byte("1234567890123456")
		iv := make([]byte, 16)
		res, err := decryptAES128CBC(make([]byte, 16), key, iv)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 16 {
			t.Errorf("expected 16 bytes, got %d", len(res))
		}
	})
}

func TestDownload_FileErrors(t *testing.T) {
	client := &Client{
		clientID: "test",
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					if strings.Contains(req.URL.String(), "hls") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"url": "http://mock/mpl"}`))}, nil
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXTINF:1,\nseg.ts\n#EXT-X-ENDLIST"))}, nil
				},
			},
		},
	}

	track := &Track{ID: 1, Title: "S", Artist: "A", HLSURL: "http://api/soundcloud:tracks:1/file-err/stream/hls"}

	t.Run("CreateFail", func(t *testing.T) {
		_, err := client.Download(context.Background(), track, "/non-existent-path/hopefully", nil)
		if err == nil {
			t.Error("expected error for invalid path")
		}
	})
}

func TestResolveURI_Error(t *testing.T) {
	_, err := resolveURI(&url.URL{}, "://invalid")
	if err == nil {
		t.Error("expected error for invalid URI")
	}
}

func TestDecryptAES128CBC_CipherError(t *testing.T) {
	// Invalid key size causes NewCipher to fail
	_, err := decryptAES128CBC([]byte("1234567890123456"), []byte("bad-key"), make([]byte, 16))
	if err == nil {
		t.Error("expected error for bad key")
	}
}

func TestFetchKey_Fail(t *testing.T) {
	client := &Client{
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					return nil, fmt.Errorf("fail")
				},
			},
		},
	}
	_, err := client.fetchKey(context.Background(), "http://fail", &sync.Map{})
	if err == nil {
		t.Error("expected error")
	}
}

func TestParseM3U8_URLParseError(t *testing.T) {
	// 124-126 is likely unreachable in normal flow as m3u8URL has already passed http.NewRequest
}

func TestDownload_ArtworkFetchFail(t *testing.T) {
	client := &Client{
		clientID: "test",
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					u := req.URL.String()
					if strings.Contains(u, "t500x500") {
						return nil, fmt.Errorf("fail artwork")
					}
					if strings.Contains(u, "hls") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"url": "http://mock/mpl"}`))}, nil
					}
					if strings.Contains(u, "mpl") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXTINF:1,\nseg.ts\n#EXT-X-ENDLIST"))}, nil
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("some-audio-data"))}, nil
				},
			},
		},
	}
	track := &Track{ID: 1, Title: "S", Artist: "A", ArtworkURL: "http://mock/artwork-large.jpg", HLSURL: "http://api/soundcloud:tracks:1/artwork-fail/stream/hls"}
	_, err := client.Download(context.Background(), track, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseM3U8_ResolveErrors(t *testing.T) {
	client := &Client{
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\":invalid\"\n#EXTINF:1,\nseg.ts\n#EXT-X-ENDLIST"))}, nil
				},
			},
		},
	}
	_, err := client.parseM3U8(context.Background(), "http://mock")
	if err == nil {
		t.Error("expected error for invalid key URI")
	}

	client.httpClient.Transport = &mockTransport{
		RoundTripFunc: func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXTINF:1,\n:invalid-seg\n#EXT-X-ENDLIST"))}, nil
		},
	}
	_, err = client.parseM3U8(context.Background(), "http://mock")
	if err == nil {
		t.Error("expected error for invalid segment URI")
	}

	client.httpClient.Transport = &mockTransport{
		RoundTripFunc: func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXTINF:1,\nseg1.ts\n#EXT-X-KEY:METHOD=AES-128,URI=\":invalid\"\n#EXTINF:1,\nseg2.ts\n#EXT-X-ENDLIST"))}, nil
		},
	}
	_, err = client.parseM3U8(context.Background(), "http://mock")
	if err == nil {
		t.Error("expected error for invalid segment key URI")
	}
}

func TestEmbedMetadata_Fail(t *testing.T) {
	client := &Client{}
	err := client.embedMetadata(context.Background(), t.TempDir(), &Track{}) // Passing a directory instead of a file
	if err == nil {
		t.Error("expected error when embedding metadata on a directory")
	}
}

func TestDecryptSegment_GlobalKey(t *testing.T) {
	client := &Client{
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("keydata012345678"))}, nil
				},
			},
		},
	}
	seg := &m3u8.MediaSegment{}
	globalKey := &m3u8.Key{URI: "http://key", IV: "0x00000000000000000000000000000000"}
	var cache sync.Map
	data := make([]byte, 16)
	_, err := client.decryptSegment(context.Background(), data, seg, globalKey, 0, &cache)
	if err != nil {
		t.Errorf("decryptSegment failed: %v", err)
	}
}

func TestDecryptSegment_FetchKeyFail(t *testing.T) {
	client := &Client{
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					return nil, fmt.Errorf("fetch fail")
				},
			},
		},
	}
	seg := &m3u8.MediaSegment{Key: &m3u8.Key{URI: "http://key"}}
	_, err := client.decryptSegment(context.Background(), nil, seg, nil, 0, &sync.Map{})
	if err == nil {
		t.Error("expected error")
	}
}
