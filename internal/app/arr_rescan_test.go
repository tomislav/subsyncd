package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/notifier"
	"subsyncd/internal/worker"
	"subsyncd/internal/workflow"
)

type recordingRescanner struct{ kind domain.MediaKind }

func (r recordingRescanner) Kind() domain.MediaKind            { return r.kind }
func (recordingRescanner) Rescan(context.Context, int64) error { return nil }

// arrRescansQueued counts the outbox's Arr rescans for movie 7 of instance tv.
func (h *rejectHarness) arrRescansQueued(t *testing.T) int {
	t.Helper()
	inspection, err := sql.Open("sqlite", filepath.Join(h.cfg.DataDir, "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Close()
	var total, forMovie int
	if err := inspection.QueryRow(`SELECT count(*), count(*) FILTER (WHERE json_extract(payload_json,'$.media.ref.instance')='tv' AND json_extract(payload_json,'$.media.entity_id')=7) FROM notifications WHERE notifier='arr_rescan' AND next_attempt_at_ns > 0`).Scan(&total, &forMovie); err != nil {
		t.Fatal(err)
	}
	if total != forMovie {
		t.Fatalf("Arr rescans for another target: %d of %d", total-forMovie, total)
	}
	return total
}

func TestUpgradeAndRejectQueueArrRescans(t *testing.T) {
	h := newRejectHarness(t, nil)
	h.rescanners = map[string]notifier.ArrRescanner{"tv": recordingRescanner{kind: domain.MediaMovie}}
	h.exec(t, `DELETE FROM notifications`)

	// An upgrade removes the old file's sidecar and installs one for the new
	// file: two changes on disk, two rescans of the movie.
	h.replaceMedia(t, "Movie.FUZEER.mkv", 8, "upgraded media")
	if outcome := h.search(t); outcome != "installed" {
		t.Fatalf("search after the upgrade = %s", outcome)
	}
	if n := h.arrRescansQueued(t); n != 2 {
		t.Fatalf("Arr rescans after the upgrade = %d, want 2", n)
	}
	h.exec(t, `DELETE FROM notifications`)

	h.provider.results = []string{"best"}
	reject := rejectArgs("en", "out_of_sync")
	reject.Selection.FileID = h.media.Ref.FileID
	if _, err := h.app.Reject(t.Context(), reject); err != nil {
		t.Fatal(err)
	}
	if n := h.arrRescansQueued(t); n != 1 {
		t.Fatalf("Arr rescans after a reject removal = %d, want 1", n)
	}
}

func TestRescanAfterChangesOffQueuesNoArrRescan(t *testing.T) {
	h := newRejectHarness(t, func(cfg *config.Config) {
		off := false
		for index := range cfg.Instances {
			cfg.Instances[index].RescanAfterChanges = &off
		}
	})
	h.rescanners = map[string]notifier.ArrRescanner{"tv": recordingRescanner{kind: domain.MediaMovie}}
	h.replaceMedia(t, "Movie.FUZEER.mkv", 8, "upgraded media")
	if outcome := h.search(t); outcome != "installed" {
		t.Fatalf("search after the upgrade = %s", outcome)
	}
	if n := h.arrRescansQueued(t); n != 0 {
		t.Fatalf("Arr rescans with rescan_after_changes off = %d", n)
	}
}

func TestBuildArrRescannersHonoursOptOut(t *testing.T) {
	off := false
	cfg := config.Config{Instances: []config.InstanceConfig{
		{Name: "sonarr-main", Type: "sonarr", URL: "http://sonarr.invalid", APIKey: "key"},
		{Name: "radarr-main", Type: "radarr", URL: "http://radarr.invalid", APIKey: "key", RescanAfterChanges: &off},
	}}
	built, err := buildArrRescanners(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 || built["sonarr-main"] == nil || built["sonarr-main"].Kind() != domain.MediaEpisode {
		t.Fatalf("built = %#v", built)
	}
	// Tests that supply catalogs get no Arr requests unless they ask.
	if built, err := buildArrRescanners(cfg, Options{Catalogs: map[string]catalog.Catalog{}}); err != nil || len(built) != 0 {
		t.Fatalf("with supplied catalogs = %#v/%v", built, err)
	}
	supplied := map[string]notifier.ArrRescanner{"sonarr-main": recordingRescanner{kind: domain.MediaEpisode}, "radarr-main": recordingRescanner{kind: domain.MediaMovie}}
	if built, err := buildArrRescanners(cfg, Options{ArrRescanners: supplied}); err != nil || len(built) != 1 || built["sonarr-main"] == nil {
		t.Fatalf("with supplied rescanners = %#v/%v", built, err)
	}
}

// With no instance rescanning, an Arr rescan queued earlier (before
// rescan_after_changes was turned off, or by a newer build) completes as
// skipped, not as a failure.
func TestLeftoverArrRescanIsSkippedWithoutRescanners(t *testing.T) {
	h := newRejectHarness(t, nil)
	h.exec(t, `DELETE FROM notifications`)
	payload, err := json.Marshal(workflow.NotificationPayload{Media: h.media, SubtitlePath: h.sidecar})
	if err != nil {
		t.Fatal(err)
	}
	h.exec(t, `INSERT INTO notifications(notifier, payload_json, attempt, next_attempt_at_ns, result, dedupe_key) VALUES ('arr_rescan', ?, 0, ?, '', 'leftover')`, payload, h.now.UnixNano())
	if err := h.app.Worker.(*worker.Worker).RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	inspection, err := sql.Open("sqlite", filepath.Join(h.cfg.DataDir, "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Close()
	var result string
	if err := inspection.QueryRow(`SELECT result FROM notifications WHERE notifier='arr_rescan'`).Scan(&result); err != nil {
		t.Fatal(err)
	}
	if result != "skipped" {
		t.Fatalf("leftover Arr rescan result = %q, want skipped", result)
	}
}
