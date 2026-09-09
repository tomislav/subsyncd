package opensubtitles

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/pack"
)

func TestBroadSearchUsesIdentityOrTitleWithoutEpisodeYear(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind domain.MediaKind
		ids  domain.ExternalIDs
		want url.Values
	}{
		{"episode IMDb", domain.MediaEpisode, domain.ExternalIDs{IMDb: "tt1234567", TMDB: 7654}, url.Values{"parent_imdb_id": {"1234567"}, "season_number": {"3"}, "episode_number": {"2"}}},
		{"episode TMDB", domain.MediaEpisode, domain.ExternalIDs{TMDB: 7654}, url.Values{"parent_tmdb_id": {"7654"}, "season_number": {"3"}, "episode_number": {"2"}}},
		{"episode title", domain.MediaEpisode, domain.ExternalIDs{}, url.Values{"query": {"Example Show"}, "season_number": {"3"}, "episode_number": {"2"}}},
		{"movie IMDb", domain.MediaMovie, domain.ExternalIDs{IMDb: "tt1234567", TMDB: 7654}, url.Values{"imdb_id": {"1234567"}}},
		{"movie TMDB", domain.MediaMovie, domain.ExternalIDs{TMDB: 7654}, url.Values{"tmdb_id": {"7654"}}},
		{"movie title", domain.MediaMovie, domain.ExternalIDs{}, url.Values{"query": {"Example Show"}, "year": {"2024"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := episodeMedia()
			m.Ref.Kind = tc.kind
			m.Season = 3
			m.ExternalIDs = tc.ids
			p := url.Values{}
			setBroadParameters(p, m)
			if p.Encode() != tc.want.Encode() {
				t.Fatalf("query = %s; want %s", p.Encode(), tc.want.Encode())
			}
		})
	}
}

func TestDownloadRequestsSRTAndProducesExtractableSubtitle(t *testing.T) {
	const subtitle = "1\n00:00:01,000 --> 00:00:02,000\nHello\n"
	var format string
	c := reviewClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/login":
			return reviewResponse(r, 200, `{"token":"test"}`), nil
		case "/api/v1/download":
			var p struct {
				FileID int64    `json:"file_id"`
				Format string   `json:"sub_format"`
				InFPS  *float64 `json:"in_fps"`
				OutFPS *float64 `json:"out_fps"`
			}
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.FileID != 42 {
				t.Fatalf("file ID = %d", p.FileID)
			}
			if p.InFPS != nil || p.OutFPS != nil {
				t.Fatal("download invents unavailable frame rates")
			}
			format = p.Format
			if format != "srt" {
				return reviewResponse(r, 200, `{"link":"https://cdn.example.test/file","file_name":"movie.sub"}`), nil
			}
			return reviewResponse(r, 200, `{"link":"https://cdn.example.test/file","file_name":"movie.srt"}`), nil
		default:
			if r.Header.Get("Api-Key") != "" || r.Header.Get("Authorization") != "" {
				t.Fatal("signed transfer leaks credentials")
			}
			if format != "srt" {
				return reviewResponse(r, 200, "{1}{1}25.000\n{25}{50}Hello\n"), nil
			}
			return reviewResponse(r, 200, subtitle), nil
		}
	})
	var b bytes.Buffer
	candidate := domain.Candidate{DownloadRef: "42", Language: "en", Kind: domain.MediaMovie}
	metadata, err := c.Download(context.Background(), candidate, &b)
	if err != nil {
		t.Fatal(err)
	}
	if format != "srt" {
		t.Fatalf("sub_format = %q; want srt", format)
	}
	candidate.DownloadRef = metadata.Filename
	if _, err = pack.Extract(context.Background(), candidate, &b, int64(b.Len()), filepath.Join(t.TempDir(), "out"), pack.DefaultLimits()); err != nil {
		t.Fatalf("converted subtitle extraction: %v", err)
	}
}

func TestReturnedLanguageAliasesCanonicalizeBeforeMapping(t *testing.T) {
	for _, p := range [][2]string{{"pt-pt", "pt"}, {"PT-pt", "pt"}, {"zh-cn", "zh"}, {"ZH-CN", "zh"}, {"EA", "es-MX"}, {"pt-br", "pt-BR"}, {"zh-Hant", "zh-Hant"}, {"es-419", "es-419"}} {
		got, err := fromOpenSubtitlesLanguage(p[0])
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != p[1] {
			t.Errorf("%s normalized to %s; want %s", p[0], got, p[1])
		}
	}
}
