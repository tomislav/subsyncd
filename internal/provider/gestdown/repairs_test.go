package gestdown

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/match"
	"subsyncd/internal/pack"
)

func TestCatalogEntrySearchDownloadAndValidation(t *testing.T) {
	for _, hi := range []bool{false, true} {
		t.Run(map[bool]string{false: "regular", true: "HI"}[hi], func(t *testing.T) {
			id := "sp_" + showID + "_entry_" + subtitleID
			response := strings.ReplaceAll(episodeJSON, subtitleID, id)
			if !hi {
				response = strings.Replace(response, `"hearingImpaired":true`, `"hearingImpaired":false`, 1)
			}
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/shows/external/tvdb/123":
					io.WriteString(w, showJSON)
				case "/subtitles/get/" + showID + "/1/2/en":
					io.WriteString(w, response)
				case "/subtitles/download/" + id:
					io.WriteString(w, subtitleText)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			got, err := c.Search(context.Background(), query())
			if err != nil || len(got) != 1 {
				t.Fatalf("search=%+v %v", got, err)
			}
			if got[0].HearingImpaired != hi || got[0].ResultID != id {
				t.Fatalf("identity/HI lost: %+v", got[0])
			}
			var body bytes.Buffer
			if _, err := c.Download(context.Background(), got[0], &body); err != nil {
				t.Fatal(err)
			}
			manifest, err := pack.Extract(context.Background(), got[0], bytes.NewReader(body.Bytes()), int64(body.Len()), filepath.Join(t.TempDir(), "extract"), pack.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pack.SelectSingleEpisode(manifest, got[0], query().Media, false); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func searchVersion(t *testing.T, version, release string, qualities []string) domain.Candidate {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal([]byte(episodeJSON), &response); err != nil {
		t.Fatal(err)
	}
	item := response["matchingSubtitles"].([]any)[0].(map[string]any)
	item["version"] = version
	item["release"] = release
	item["qualities"] = qualities
	item["downloadCount"] = 0
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shows/external/tvdb/123" {
			io.WriteString(w, showJSON)
		} else if r.URL.Path == "/subtitles/get/"+showID+"/1/2/en" {
			w.Write(payload)
		} else {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	got, err := c.Search(context.Background(), query())
	if err != nil || len(got) != 1 {
		t.Fatalf("search=%+v %v", got, err)
	}
	return got[0]
}

func signalPoints(score domain.Score, signal string) int {
	for _, c := range score.Contributions {
		if c.Signal == signal {
			return c.Points
		}
	}
	return 0
}

func TestCommaSeparatedVersionsHaveOrderIndependentScores(t *testing.T) {
	for _, version := range []string{"WEB-DL-GROUPA, BluRay-GROUPB", "BluRay-GROUPB, WEB-DL-GROUPA"} {
		candidate := searchVersion(t, version, "", nil)
		media := query().Media
		media.ReleaseGroup = "GROUPB"
		media.Source = "bluray"
		score := match.Evaluate(media, candidate, "en")
		if score.Total != 80 {
			t.Fatalf("%q score=%+v; want identity 20 + episode 20 + group 25 + source 15", version, score)
		}
	}
}

func TestProviderVersionGroupEvidence(t *testing.T) {
	for _, tc := range []struct {
		version, group string
		want           int
	}{
		{"0tv", "0tv", 25}, {"DVDRip ORPHEUS", "ORPHEUS", 25}, {"BluRayREWARD", "REWARD", 25},
		{"LOL", "DIMENSION", 25}, {"WEB-DL-GROUP", "GROUP", 25},
		{"NOTORPHEUS", "ORPHEUS", 0}, {"ORPHEUSfake", "ORPHEUS", 0}, {"BluRayREWARDfake", "REWARD", 0},
		{"Example Show", "Show", 0}, {"720p", "720p", 0}, {"WEB-DL", "DL", 0}, {"1080p", "1080p", 0},
		{"x264", "x264", 0}, {"BluRay", "BluRay", 0},
	} {
		t.Run(tc.version+"/"+tc.group, func(t *testing.T) {
			candidate := searchVersion(t, tc.version, "", nil)
			media := query().Media
			media.ReleaseGroup = tc.group
			score := match.Evaluate(media, candidate, "en")
			if got := signalPoints(score, "release_group"); got != tc.want {
				t.Fatalf("group points=%d want=%d score=%+v", got, tc.want, score)
			}
			if candidate.Year != 0 || candidate.ExternalIDs.IMDb != "" {
				t.Fatal("invented query evidence")
			}
		})
	}
}

func TestExplicitQualitiesMatchWithoutInventingResolution(t *testing.T) {
	for _, tc := range []struct {
		name      string
		qualities []string
		target    string
		want      int
	}{
		{"multiple", []string{"720p", "1080p"}, "1080p", 5},
		{"other supported", []string{"720p", "1080p"}, "720p", 5},
		{"unknown target", []string{"720p", "1080p"}, "2160p", 0},
		{"empty", nil, "1080p", 0}, {"unknown label", []string{"HD", "4K"}, "1080p", 0},
		{"unknown value", []string{"garbage"}, "garbage", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := searchVersion(t, "Bluray-CtrlHD", "", tc.qualities)
			media := query().Media
			media.Resolution = tc.target
			score := match.Evaluate(media, candidate, "en")
			if got := signalPoints(score, "resolution"); got != tc.want {
				t.Fatalf("resolution points=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestSplitVersionsPreserveFullReleaseAndDeduplicate(t *testing.T) {
	candidate := searchVersion(t, " LOL, DIMENSION, LOL, ", "Example, Show.S01E02.1080p.WEB-DL-GROUP", []string{"720p", "1080p", "1080p"})
	if len(candidate.ReleaseNames) != 3 || candidate.ReleaseNames[0] != "Example, Show.S01E02.1080p.WEB-DL-GROUP" {
		t.Fatalf("full release split or duplicates retained: %+v", candidate.ReleaseNames)
	}
	if len(candidate.ReleaseGroups) != 2 || len(candidate.Resolutions) != 2 {
		t.Fatalf("duplicate explicit evidence: %+v", candidate)
	}
}
