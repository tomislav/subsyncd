package titlovi

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"subsyncd/internal/domain"
	"subsyncd/internal/match"
	baseprovider "subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
	"testing"
	"time"
)

func TestReturnedAlternateTitleContributesIdentity(t *testing.T) {
	c := &Client{id: "titlovi"}
	media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Title: "English Movie", Year: 2024, Source: "WEB-DL", Resolution: "1080p"}
	got := c.normalize(baseprovider.SearchQuery{Media: media, Language: "hr"}, []searchItem{{ID: 1, Language: "Hrvatski", Link: "/download/1", Title: "Original Name AKA English Movie", Year: 2024, Release: "1080p.WEB-DL"}})
	score := match.Evaluate(media, got[0], "hr")
	corrected := got[0]
	corrected.Title = "English Movie"
	withAKA := match.Evaluate(media, corrected, "hr")
	t.Logf("title=%q actual=%d eligible=%v with provider AKA=%d eligible=%v", got[0].Title, score.Total, match.Eligible(score, 35), withAKA.Total, match.Eligible(withAKA, 35))
	if got[0].Title != "Original Name" || score.Total != 35 || score.Total != withAKA.Total {
		t.Errorf("provider-supplied alternate title discarded: %d vs %d", score.Total, withAKA.Total)
	}
}

func TestDownloadPreservesAllowedReturnedOrigin(t *testing.T) {
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	gate := baseprovider.NewGate(&testStateStore{states: map[string]store.ProviderState{}}, clock, 1)
	gate.Configure("titlovi", 1000, 10, 1)
	var requested string
	transport := baseprovider.Client{HTTP: &http.Client{Transport: reviewTransport(func(r *http.Request) (*http.Response, error) {
		requested = r.URL.String()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("subtitle")), Request: r}, nil
	})}, Gate: gate, Clock: clock, ProviderID: "titlovi", ProviderType: "titlovi"}
	c, err := New(Config{Username: "user", Password: "pass"}, transport, clock)
	if err != nil {
		t.Fatal(err)
	}
	c.token = "token"
	c.userID = 42
	c.expiresAt = clock.Now().Add(time.Hour)
	original := "https://kodi.titlovi.com/download/123"
	got := c.normalize(baseprovider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}}, Language: "hr"}, []searchItem{{ID: 1, Language: "Hrvatski", Link: original, Title: "Example"}})
	if len(got) != 1 {
		t.Fatal("candidate omitted")
	}
	_, err = c.Download(context.Background(), got[0], io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("provider URL=%s normalized=%s requested=%s", original, got[0].DownloadRef, requested)
	if requested != original {
		t.Errorf("allowed provider download origin changed")
	}
}

func TestReturnedTitleEvidenceDoesNotUseQueryIdentity(t *testing.T) {
	client := &Client{id: "titlovi"}
	query := baseprovider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Title: "Unrelated Query", Year: 2024, ExternalIDs: domain.ExternalIDs{IMDb: "tt1234567"}}, Language: "hr"}
	for _, test := range []struct {
		title        string
		primary      string
		alternatives []string
	}{
		{" Original Name aKa English Movie AKA Local Movie ", "Original Name", []string{"English Movie", "Local Movie"}},
		{"Original Name", "Original Name", nil},
		{"", "", nil},
	} {
		got := client.normalize(query, []searchItem{{ID: 1, Language: "Hrvatski", Link: "/download/1", Title: test.title}})
		if len(got) != 1 {
			t.Fatal("candidate omitted")
		}
		if got[0].Title != test.primary || !reflect.DeepEqual(got[0].AlternateTitles, test.alternatives) || got[0].Year != 0 || got[0].ExternalIDs.IMDb != "" {
			t.Fatalf("unexpected returned identity: %+v", got[0])
		}
	}
}
