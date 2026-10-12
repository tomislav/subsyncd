package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"subsyncd/internal/catalog"
	"subsyncd/internal/domain"
)

func sonarrEpisode(seriesID int64) domain.Media {
	return domain.Media{SeriesID: seriesID, EntityID: 100 + seriesID, Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 7}, Fingerprint: domain.MediaFingerprint{Path: "/media/tv/Show/S01E01.mkv"}}
}

func radarrMovie(movieID int64) domain.Media {
	return domain.Media{EntityID: movieID, Ref: domain.MediaRef{Instance: "radarr-main", Kind: domain.MediaMovie, FileID: 9}, Fingerprint: domain.MediaFingerprint{Path: "/media/movies/Movie/Movie.mkv"}}
}

type arrCommandServer struct {
	*httptest.Server
	mu       sync.Mutex
	commands []map[string]any
	status   int
}

func newArrCommandServer(t *testing.T, status int) *arrCommandServer {
	t.Helper()
	server := &arrCommandServer{status: status}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v3/command" || r.Header.Get("X-Api-Key") != "arr-secret" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var command map[string]any
		if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
			t.Error(err)
		}
		server.mu.Lock()
		server.commands = append(server.commands, command)
		server.mu.Unlock()
		w.WriteHeader(server.status)
		_, _ = w.Write([]byte(`{"message":"body-secret"}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func arrRescanFor(t *testing.T, instance, instanceType, url string) *ArrRescan {
	t.Helper()
	client, err := catalog.NewArrRescanClient(instance, instanceType, url, "arr-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	return NewArrRescan(map[string]ArrRescanner{instance: client})
}

func TestArrRescanSendsSonarrAndRadarrCommands(t *testing.T) {
	sonarr := newArrCommandServer(t, http.StatusCreated)
	radarr := newArrCommandServer(t, http.StatusCreated)
	sonarrClient, err := catalog.NewArrRescanClient("sonarr-main", "sonarr", sonarr.URL, "arr-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	radarrClient, err := catalog.NewArrRescanClient("radarr-main", "radarr", radarr.URL, "arr-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	rescan := NewArrRescan(map[string]ArrRescanner{"sonarr-main": sonarrClient, "radarr-main": radarrClient})
	if err := rescan.SubtitleChanged(t.Context(), sonarrEpisode(12), "/media/tv/Show/S01E01.en.srt"); err != nil {
		t.Fatalf("Sonarr delivery error = %v", err)
	}
	if err := rescan.SubtitleChanged(t.Context(), radarrMovie(34), "/media/movies/Movie/Movie.en.srt"); err != nil {
		t.Fatalf("Radarr delivery error = %v", err)
	}
	if len(sonarr.commands) != 1 || sonarr.commands[0]["name"] != "RescanSeries" || sonarr.commands[0]["seriesId"] != float64(12) || len(sonarr.commands[0]) != 2 {
		t.Fatalf("Sonarr commands = %#v", sonarr.commands)
	}
	if len(radarr.commands) != 1 || radarr.commands[0]["name"] != "RescanMovie" || radarr.commands[0]["movieId"] != float64(34) || len(radarr.commands[0]) != 2 {
		t.Fatalf("Radarr commands = %#v", radarr.commands)
	}
}

func TestArrRescanClassifiesFailures(t *testing.T) {
	for _, test := range []struct {
		status    int
		retryable bool
		refused   bool
	}{
		{status: http.StatusNotFound, refused: true},
		{status: http.StatusBadRequest, refused: true},
		{status: http.StatusUnauthorized, refused: true},
		{status: http.StatusTooManyRequests, retryable: true},
		{status: http.StatusInternalServerError, retryable: true},
		{status: http.StatusServiceUnavailable, retryable: true},
	} {
		server := newArrCommandServer(t, test.status)
		err := arrRescanFor(t, "sonarr-main", "sonarr", server.URL).SubtitleChanged(t.Context(), sonarrEpisode(5), "")
		var delivery *DeliveryError
		if !errors.As(err, &delivery) || delivery.StatusCode != test.status || IsRetryable(err) != test.retryable || IsRefused(err) != test.refused {
			t.Fatalf("status %d: err = %#v", test.status, err)
		}
		for _, leaked := range []string{"arr-secret", "body-secret", server.URL} {
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("error leaked %q: %v", leaked, err)
			}
		}
	}
}

func TestArrRescanRetriesUnreachableArr(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	err := arrRescanFor(t, "radarr-main", "radarr", url).SubtitleChanged(t.Context(), radarrMovie(3), "")
	if !IsRetryable(err) || IsRefused(err) || strings.Contains(err.Error(), url) {
		t.Fatalf("err = %v", err)
	}
}

func TestArrRescanSkipsDisabledInstanceAndUnknownID(t *testing.T) {
	server := newArrCommandServer(t, http.StatusCreated)
	rescan := arrRescanFor(t, "sonarr-main", "sonarr", server.URL)
	other := sonarrEpisode(5)
	other.Ref.Instance = "sonarr-4k"
	for media, want := range map[*domain.Media]string{&other: SkipRescanDisabled, func() *domain.Media { m := sonarrEpisode(0); return &m }(): SkipUnknownArrID} {
		reason, skipped := IsSkipped(rescan.SubtitleChanged(t.Context(), *media, ""))
		if !skipped || reason != want {
			t.Fatalf("%s: skipped=%v reason=%q", media.Ref.Instance, skipped, reason)
		}
	}
	if len(server.commands) != 0 {
		t.Fatalf("skipped deliveries sent %#v", server.commands)
	}
}

func TestArrRescanPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := newArrCommandServer(t, http.StatusCreated)
	err := arrRescanFor(t, "sonarr-main", "sonarr", server.URL).SubtitleChanged(ctx, sonarrEpisode(5), "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestArrRescanCoalescesPerInstanceAndSeriesOrMovie(t *testing.T) {
	rescan := NewArrRescan(nil)
	key := func(media domain.Media) string {
		t.Helper()
		value, err := rescan.CoalesceKey(media, "")
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	episode := sonarrEpisode(5)
	sibling := sonarrEpisode(5)
	sibling.EntityID, sibling.Season, sibling.Fingerprint.Path = 999, 2, "/media/tv/Show/Season 02/S02E01.mkv"
	if key(episode) != key(sibling) {
		t.Fatal("episodes of one series do not coalesce")
	}
	if key(episode) == key(sonarrEpisode(6)) {
		t.Fatal("different series coalesce")
	}
	otherInstance := sonarrEpisode(5)
	otherInstance.Ref.Instance = "sonarr-4k"
	if key(episode) == key(otherInstance) {
		t.Fatal("same series ID of different instances coalesce")
	}
	if key(radarrMovie(5)) == key(episode) || key(radarrMovie(5)) != key(radarrMovie(5)) {
		t.Fatal("movie key is wrong")
	}
	if _, err := rescan.CoalesceKey(sonarrEpisode(0), ""); err == nil {
		t.Fatal("unknown series ID coalesced")
	}
}

func TestArrRescanDeliveryAttrsNameTheTarget(t *testing.T) {
	rescan := NewArrRescan(nil)
	got := map[string]string{}
	for _, attr := range rescan.DeliveryAttrs(sonarrEpisode(5)) {
		got[attr.Key] = attr.Value.String()
	}
	if got["instance"] != "sonarr-main" || got["media_kind"] != "episode" || got["series_id"] != "5" || len(got) != 3 {
		t.Fatalf("episode attrs = %#v", got)
	}
	got = map[string]string{}
	for _, attr := range rescan.DeliveryAttrs(radarrMovie(8)) {
		got[attr.Key] = attr.Value.String()
	}
	if got["movie_id"] != "8" || got["media_kind"] != "movie" || len(got) != 3 {
		t.Fatalf("movie attrs = %#v", got)
	}
}
