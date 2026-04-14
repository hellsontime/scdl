package scdl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGetTrack(t *testing.T) {
	// Sample hydration data (minified)
	hydrationData := `[{"hydratable":"user","data":{}},{"hydratable":"sound","data":{"id":123456,"title":"Test Title","description":"Test Description","genre":"Rock","duration":60000,"artwork_url":"https://i1.sndcdn.com/artworks-000-large.jpg","track_authorization":"auth-token","user":{"username":"Test Artist"},"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/soundcloud:tracks:123456/stream/hls","format":{"protocol":"hls","mime_type":"audio/mpeg"}},{"url":"https://api-v2.soundcloud.com/media/soundcloud:tracks:123456/stream/progressive","format":{"protocol":"progressive","mime_type":"audio/mpeg"}}]}}}]`

	htmlResponse := `
	<html>
	<body>
	<script>
	window.__sc_hydration = ` + hydrationData + `;
	</script>
	</body>
	</html>
	`

	client := &Client{
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					if req.URL.String() == "https://soundcloud.com/artist/song" {
						return &http.Response{
							StatusCode: 200,
							Body:       io.NopCloser(strings.NewReader(htmlResponse)),
							Header:     make(http.Header),
						}, nil
					}
					return &http.Response{
						StatusCode: 404,
						Body:       io.NopCloser(strings.NewReader("Not Found")),
					}, nil
				},
			},
		},
	}

	track, err := client.GetTrack(context.Background(), "https://soundcloud.com/artist/song")
	if err != nil {
		t.Fatalf("GetTrack() error = %v", err)
	}

	if track.ID != 123456 {
		t.Errorf("got ID %d, want 123456", track.ID)
	}
	if track.Title != "Test Title" {
		t.Errorf("got Title %q, want %q", track.Title, "Test Title")
	}
	if track.Artist != "Test Artist" {
		t.Errorf("got Artist %q, want %q", track.Artist, "Test Artist")
	}
	if track.HLSURL != "https://api-v2.soundcloud.com/media/soundcloud:tracks:123456/stream/hls" {
		t.Errorf("got HLSURL %q, want %q", track.HLSURL, "https://api-v2.soundcloud.com/media/soundcloud:tracks:123456/stream/hls")
	}
}
func TestGetTrack_Errors(t *testing.T) {
	client := &Client{
		httpClient: &http.Client{
			Transport: &mockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					u := req.URL.String()
					if strings.Contains(u, "fail") {
						return nil, fmt.Errorf("fail")
					}
					if strings.Contains(u, "no-hydration") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`no data`))}, nil
					}
					if strings.Contains(u, "bad-json") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`window.__sc_hydration = [invalid];`))}, nil
					}
					if strings.Contains(u, "no-sound") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`window.__sc_hydration = [{"hydratable":"user","data":{}}];`))}, nil
					}
					if strings.Contains(u, "no-hls") {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`window.__sc_hydration = [{"hydratable":"sound","data":{"id":1,"media":{"transcodings":[]}}}];`))}, nil
					}
					return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("404"))}, nil
				},
			},
		},
	}

	t.Run("FetchFail", func(t *testing.T) {
		_, err := client.GetTrack(context.Background(), "http://fail")
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("NoHydration", func(t *testing.T) {
		_, err := client.GetTrack(context.Background(), "http://no-hydration")
		if err == nil || !strings.Contains(err.Error(), "hydration data not found") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("BadJSON", func(t *testing.T) {
		_, err := client.GetTrack(context.Background(), "http://bad-json")
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("BadDataJSON", func(t *testing.T) {
		localClient := &Client{
			httpClient: &http.Client{
				Transport: &mockTransport{
					RoundTripFunc: func(req *http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`window.__sc_hydration = [{"hydratable":"sound","data":{"id":"not-an-int"}}];`))}, nil
					},
				},
			},
		}
		_, err := localClient.GetTrack(context.Background(), "http://bad-data-json")
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("NoSound", func(t *testing.T) {
		_, err := client.GetTrack(context.Background(), "http://no-sound")
		if err == nil || !strings.Contains(err.Error(), "no sound entry found") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("NoHLS", func(t *testing.T) {
		_, err := client.GetTrack(context.Background(), "http://no-hls")
		if err == nil || !strings.Contains(err.Error(), "no HLS audio/mpeg transcoding found") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestCleanupTrackTitle(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		artist   string
		album    string
		expected string
	}{
		{
			name:     "No Changes Needed",
			title:    "My Awesome Song",
			artist:   "Joe",
			album:    "The Album",
			expected: "My Awesome Song",
		},
		{
			name:     "Empty Artist And Album",
			title:    "My Awesome Song - Joe",
			artist:   "",
			album:    "",
			expected: "My Awesome Song - Joe",
		},
		{
			name:     "Exact Match Artist",
			title:    "Joe - My Awesome Song",
			artist:   "Joe",
			album:    "The Album",
			expected: "My Awesome Song",
		},
		{
			name:     "Exact Match Album",
			title:    "My Awesome Song - The Album",
			artist:   "Joe",
			album:    "The Album",
			expected: "My Awesome Song",
		},
		{
			name:     "Artist And Album Removed",
			title:    "Joe - My Awesome Song - The Album",
			artist:   "Joe",
			album:    "The Album",
			expected: "My Awesome Song",
		},
		{
			name:     "Case Insensitive Match",
			title:    "my awesome song - JOE",
			artist:   "joe",
			album:    "the album",
			expected: "my awesome song",
		},
		{
			name:     "Ampersand List Match First",
			title:    "Joe & Steve - My Awesome Song",
			artist:   "Joe",
			album:    "The Album",
			expected: "My Awesome Song",
		},
		{
			name:     "Ampersand List Match Second",
			title:    "Steve & Joe - My Awesome Song",
			artist:   "Joe",
			album:    "The Album",
			expected: "My Awesome Song",
		},
		{
			name:     "Comma List Match",
			title:    "My Awesome Song - Steve, Joe",
			artist:   "Joe",
			album:    "The Album",
			expected: "My Awesome Song",
		},
		{
			name:     "And List Match",
			title:    "Steve and Joe - My Awesome Song",
			artist:   "Joe",
			album:    "The Album",
			expected: "My Awesome Song",
		},
		{
			name:     "Similar Album Substring Not Removed",
			title:    "My Awesome Song - The Album Deluxe",
			artist:   "Joe",
			album:    "The Album",
			expected: "My Awesome Song - The Album Deluxe",
		},
		{
			name:     "Multiple Empty Separators",
			title:    "Joe - - My Awesome Song",
			artist:   "Joe",
			album:    "The Album",
			expected: "My Awesome Song",
		},
		{
			name:     "Hyphenated Title Preserved",
			title:    "Joe - Part-One - The Album",
			artist:   "Joe",
			album:    "The Album",
			expected: "Part-One",
		},
		{
			name:     "Artist Contains Ampersand",
			title:    "Joe & Steve - My Awesome Song",
			artist:   "Joe & Steve",
			album:    "The Album",
			expected: "My Awesome Song",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanupTrackTitle(tt.title, tt.artist, tt.album)
			if got != tt.expected {
				t.Errorf("cleanupTrackTitle(%q, %q, %q) = %q; want %q",
					tt.title, tt.artist, tt.album, got, tt.expected)
			}
		})
	}
}

func TestNullableTime(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantZero  bool
		wantYear  int
		wantError bool
	}{
		{
			name:     "null JSON value",
			input:    `null`,
			wantZero: true,
		},
		{
			name:     "empty string",
			input:    `""`,
			wantZero: true,
		},
		{
			name:     "valid RFC3339 timestamp",
			input:    `"2023-06-15T12:00:00Z"`,
			wantZero: false,
			wantYear: 2023,
		},
		{
			name:      "invalid value",
			input:     `"not-a-date"`,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var nt nullableTime
			err := json.Unmarshal([]byte(tt.input), &nt)
			if tt.wantError {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantZero && !nt.IsZero() {
				t.Errorf("expected zero time, got %v", nt.Time)
			}
			if !tt.wantZero && nt.Year() != tt.wantYear {
				t.Errorf("expected year %d, got %d", tt.wantYear, nt.Year())
			}
		})
	}

	t.Run("unmarshals correctly inside struct", func(t *testing.T) {
		var data struct {
			CreatedAt   nullableTime `json:"created_at"`
			ReleaseDate nullableTime `json:"release_date"`
		}
		err := json.Unmarshal([]byte(`{"created_at":"2021-03-01T00:00:00Z","release_date":null}`), &data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if data.CreatedAt.Year() != 2021 {
			t.Errorf("expected 2021, got %d", data.CreatedAt.Year())
		}
		if !data.ReleaseDate.IsZero() {
			t.Errorf("expected zero time for null release_date, got %v", data.ReleaseDate.Time)
		}
	})

	t.Run("empty string in struct does not error", func(t *testing.T) {
		var data struct {
			ReleaseDate nullableTime `json:"release_date"`
		}
		err := json.Unmarshal([]byte(`{"release_date":""}`), &data)
		if err != nil {
			t.Fatalf("empty string should not cause unmarshal error, got: %v", err)
		}
		if !data.ReleaseDate.IsZero() {
			t.Errorf("expected zero time for empty string, got %v", data.ReleaseDate.Time)
		}
	})

}
