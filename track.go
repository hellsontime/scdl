package scdl

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Track holds metadata for a SoundCloud track.
type Track struct {
	ID                 int
	Title              string
	Album              string
	Artist             string
	ArtworkURL         string
	ArtistAvatarURL    string
	Genre              string
	Description        string
	Year               string
	Duration           int // milliseconds
	TrackAuthorization string
	HLSURL             string // HLS transcoding URL for audio/mpeg
}

var hydrationRe = regexp.MustCompile(`window\.__sc_hydration\s*=\s*(\[.+?]);`)

// nullableTime is a time.Time that safely handles null or empty string JSON values.
type nullableTime struct {
	time.Time
}

func (t *nullableTime) UnmarshalJSON(data []byte) error {
	s := string(data)
	if s == "null" || s == `""` {
		return nil
	}
	return json.Unmarshal(data, &t.Time)
}

// GetTrack fetches a SoundCloud track page and extracts metadata from the
// hydration data embedded in the HTML.
func (c *Client) GetTrack(ctx context.Context, trackURL string) (*Track, error) {
	body, err := c.get(ctx, trackURL)
	if err != nil {
		return nil, fmt.Errorf("fetch track page: %w", err)
	}

	matches := hydrationRe.FindSubmatch(body)
	if len(matches) < 2 {
		return nil, fmt.Errorf("hydration data not found on page")
	}

	var hydration []struct {
		Hydratable string          `json:"hydratable"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(matches[1], &hydration); err != nil {
		return nil, fmt.Errorf("parse hydration JSON: %w", err)
	}

	for _, entry := range hydration {
		if entry.Hydratable != "sound" {
			continue
		}

		var data struct {
			ID                 int       `json:"id"`
			Title              string    `json:"title"`
			CreatedAt          nullableTime `json:"created_at"`
			ReleaseDate        nullableTime `json:"release_date"`
			Description        string    `json:"description"`
			Genre              string    `json:"genre"`
			Duration           int       `json:"duration"`
			ArtworkURL         string    `json:"artwork_url"`
			TrackAuthorization string    `json:"track_authorization"`
			PublisherMetadata  struct {
				Artist       string `json:"artist"`
				AlbumTitle   string `json:"album_title"`
				ReleaseTitle string `json:"release_title"`
			} `json:"publisher_metadata"`
			User struct {
				AvatarURL string `json:"avatar_url"`
				Username  string `json:"username"`
			} `json:"user"`
			Media struct {
				Transcodings []struct {
					URL    string `json:"url"`
					Format struct {
						Protocol string `json:"protocol"`
						MimeType string `json:"mime_type"`
					} `json:"format"`
				} `json:"transcodings"`
			} `json:"media"`
		}
		if err := json.Unmarshal(entry.Data, &data); err != nil {
			return nil, fmt.Errorf("parse track data: %w", err)
		}

		artist := data.User.Username
		if data.PublisherMetadata.Artist != "" {
			artist = data.PublisherMetadata.Artist
		}

		title := data.Title
		if data.PublisherMetadata.ReleaseTitle != "" {
			title = data.PublisherMetadata.ReleaseTitle
		} else {
			title = cleanupTrackTitle(title, artist, data.PublisherMetadata.AlbumTitle)
		}

		var year string
		if !data.ReleaseDate.IsZero() {
			year = data.ReleaseDate.Format("2006")
		} else if !data.CreatedAt.IsZero() {
			year = data.CreatedAt.Format("2006")
		}

		track := &Track{
			ID:                 data.ID,
			Title:              title,
			Album:              data.PublisherMetadata.AlbumTitle,
			Artist:             artist,
			ArtworkURL:         data.ArtworkURL,
			ArtistAvatarURL:    data.User.AvatarURL,
			Genre:              data.Genre,
			Description:        data.Description,
			Year:               year,
			Duration:           data.Duration,
			TrackAuthorization: data.TrackAuthorization,
		}

		for _, t := range data.Media.Transcodings {
			if t.Format.MimeType == "audio/mpeg" && t.Format.Protocol == "hls" {
				track.HLSURL = t.URL
				break
			}
		}

		if track.HLSURL == "" {
			return nil, fmt.Errorf("no HLS audio/mpeg transcoding found")
		}

		return track, nil
	}

	return nil, fmt.Errorf("no sound entry found in hydration data")
}

// cleanupTrackTitle cleanly removes the artist and album name from the title
// while respecting hyphenated titles (e.g. "Artist - Album - Example-Title")
func cleanupTrackTitle(title, artist, album string) string {
	if artist == "" && album == "" {
		return title
	}

	var (
		start    int
		segments []string
		removed  bool
	)

	for i := range len(title) + 1 {
		if i == len(title) || isTrackTitleSeparator(title, i) {
			trimmed := strings.TrimSpace(title[start:i])
			if trimmed != "" {
				if matchesAnyFold(trimmed, artist, album) {
					removed = true
				} else {
					segments = append(segments, trimmed)
				}
			} else {
				removed = true
			}

			start = i + 1
		}
	}

	if len(segments) == 0 || !removed {
		return title
	}

	return strings.Join(segments, " - ")
}

func isTrackTitleSeparator(title string, i int) bool {
	if title[i] != '-' {
		return false
	}

	return (i > 0 && title[i-1] == ' ') || (i+1 < len(title) && title[i+1] == ' ')
}

func matchesAnyFold(str, artist, album string) bool {
	if album != "" && strings.EqualFold(str, album) {
		return true
	}

	if artist == "" {
		return false
	}

	if strings.EqualFold(str, artist) {
		return true
	}

	var start int

	for i := 0; i < len(str); {
		isSep := false
		sepLen := 1

		if str[i] == '&' || str[i] == ',' {
			isSep = true
		} else if str[i] == ' ' && i+5 <= len(str) && strings.EqualFold(str[i:i+5], " and ") {
			isSep = true
			sepLen = 5
		}

		if isSep {
			part := strings.TrimSpace(str[start:i])
			if part != "" && strings.EqualFold(part, artist) {
				return true
			}

			start = i + sepLen
			i = start
		} else {
			i++
		}
	}

	if start < len(str) {
		part := strings.TrimSpace(str[start:])
		if part != "" && strings.EqualFold(part, artist) {
			return true
		}
	}

	return false
}
