package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/notifier"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

// newRescanFixture is a reconcile fixture whose Radarr instance has
// rescan_after_changes on, with an empty outbox.
func newRescanFixture(t *testing.T, mediaName string) *reconcileFixture {
	t.Helper()
	f := newReconcileFixture(t, mediaName)
	f.installer.ArrRescanInstances = map[string]bool{"radarr": true}
	return f
}

// queued lists the outbox as notifier names, sorted by id, and checks every
// Arr rescan names movie 1 of instance radarr.
func (f *reconcileFixture) queued(t *testing.T) []string {
	t.Helper()
	rows, err := f.audit.Query(`SELECT notifier, payload_json FROM notifications WHERE next_attempt_at_ns > 0 ORDER BY notifier, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		var payload []byte
		if err := rows.Scan(&name, &payload); err != nil {
			t.Fatal(err)
		}
		var decoded NotificationPayload
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatal(err)
		}
		if id, ok := notifier.RescanID(decoded.Media); name == notifier.ArrRescanName && (!ok || id != 1 || decoded.Media.Ref.Instance != "radarr") {
			t.Fatalf("Arr rescan payload = %#v", decoded.Media)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func (f *reconcileFixture) clearOutbox(t *testing.T) {
	t.Helper()
	if _, err := f.audit.Exec(`DELETE FROM notifications`); err != nil {
		t.Fatal(err)
	}
}

func (f *reconcileFixture) install(t *testing.T, language domain.Language, content string) error {
	t.Helper()
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), content)
	destination := strings.TrimSuffix(f.media.Fingerprint.Path, filepath.Ext(f.media.Fingerprint.Path)) + "." + language.String() + ".srt"
	_, err := f.installer.Install(f.ctx, InstallRequest{MediaID: f.mediaID, Media: f.media, Language: language, SourcePath: source, DestinationPath: destination, Candidate: domain.Candidate{ProviderID: "provider", ResultID: "candidate-" + string(language) + content[len(content)-4:]}, Score: domain.Score{Total: 80}, SyncResult: domain.SyncResult{Verdict: "solid"}})
	return err
}

func TestArrRescanQueuedForNewAndReplacementInstalls(t *testing.T) {
	f := newRescanFixture(t, "Movie.mkv")
	if err := f.install(t, "hr", installSRT); err != nil {
		t.Fatal(err)
	}
	if got := f.queued(t); !reflect.DeepEqual(got, []string{"arr_rescan", "silo"}) {
		t.Fatalf("new install queued %q", got)
	}
	f.clearOutbox(t)
	if err := f.install(t, "en", "1\n00:00:01,000 --> 00:00:02,000\nHello again\n"); err != nil {
		t.Fatal(err)
	}
	if got := f.queued(t); !reflect.DeepEqual(got, []string{"arr_rescan", "silo"}) {
		t.Fatalf("replacement queued %q", got)
	}
}

func TestArrRescanNotQueuedForSameBytesProvenanceRefresh(t *testing.T) {
	f := newRescanFixture(t, "Movie.mkv")
	if err := f.install(t, "en", f.content); err != nil {
		t.Fatal(err)
	}
	if got := f.queued(t); len(got) != 0 {
		t.Fatalf("provenance refresh queued %q", got)
	}
}

func TestArrRescanQueuedForRejectRemoval(t *testing.T) {
	f := newRescanFixture(t, "Movie.mkv")
	if err := f.installer.RemoveManaged(f.ctx, RemovalRequest{Installation: f.installation(t), Media: f.media, Manual: true}); err != nil {
		t.Fatal(err)
	}
	assertGone(t, f.sidecar)
	if got := f.queued(t); !reflect.DeepEqual(got, []string{"arr_rescan", "silo"}) {
		t.Fatalf("removal queued %q", got)
	}
}

func TestArrRescanQueuedForReplacedMediaCleanup(t *testing.T) {
	f := newRescanFixture(t, "Movie.WADU.mkv")
	f.import_(t, "import", f.mediaFile(t, "Movie.FUZEER.mkv", 8, "upgraded media"))
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	if got := f.queued(t); !reflect.DeepEqual(got, []string{"arr_rescan", "silo"}) {
		t.Fatalf("replaced-media cleanup queued %q", got)
	}
}

func TestArrRescanQueuedAloneForRenameMove(t *testing.T) {
	f := newRescanFixture(t, "Movie.Old.mkv")
	renamed := f.media
	renamed.Fingerprint.Path = filepath.Join(f.root, "Movie.New.mkv")
	if err := os.Rename(f.media.Fingerprint.Path, renamed.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	f.import_(t, "rename", renamed)
	f.logs.Reset()
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	if got := f.installation(t); got.Path != f.sidecarFor(renamed) {
		t.Fatalf("installation = %#v", got)
	}
	// Silo was not told about a moved sidecar before; only Arr is.
	if got := f.queued(t); !reflect.DeepEqual(got, []string{"arr_rescan"}) {
		t.Fatalf("rename move queued %q", got)
	}
	if record := f.event(t, "notification.queued"); record["notifier"] != "arr_rescan" {
		t.Fatalf("notification.queued = %#v", record)
	}
}

func TestArrRescanOptOutQueuesOnlyConfiguredNotifiers(t *testing.T) {
	f := newReconcileFixture(t, "Movie.mkv")
	f.installer.ArrRescanInstances = map[string]bool{"sonarr": true}
	if err := f.installer.RemoveManaged(f.ctx, RemovalRequest{Installation: f.installation(t), Media: f.media, Manual: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.queued(t); !reflect.DeepEqual(got, []string{"silo"}) {
		t.Fatalf("opted-out removal queued %q", got)
	}
}

func TestArrRescanSkippedForUnknownSeriesID(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	installer := Installer{NotifierNames: []string{"silo"}, ArrRescanInstances: map[string]bool{"sonarr": true}, Events: events}
	episode := domain.Media{EntityID: 11, Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaEpisode, FileID: 3}}
	if got := installer.changeNotifierNames(context.Background(), episode); !reflect.DeepEqual(got, []string{"silo"}) {
		t.Fatalf("unknown series ID names = %q", got)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
		t.Fatalf("logs = %q: %v", logs.String(), err)
	}
	if record["event"] != "notification.skipped" || record["notifier"] != "arr_rescan" || record["instance"] != "sonarr" || record["reason"] != "unknown_arr_id" || record["level"] != "info" {
		t.Fatalf("skip record = %#v", record)
	}
	episode.SeriesID = 5
	if got := installer.changeNotifierNames(context.Background(), episode); !reflect.DeepEqual(got, []string{"silo", "arr_rescan"}) {
		t.Fatalf("known series ID names = %q", got)
	}
	if got := installer.NotifierNames; !reflect.DeepEqual(got, []string{"silo"}) {
		t.Fatalf("configured names changed: %q", got)
	}
}

// Episodes of one series installed minutes apart while its rescan is still
// queued add no rescan and do not push it later; a change while the rescan
// is in flight gets its own; another series always does. Silo is told about
// every change, as before.
func TestArrRescansCoalescePerSeriesAcrossChanges(t *testing.T) {
	f := newReconcileFixture(t, "Movie.mkv")
	f.installer.ArrRescanInstances = map[string]bool{"sonarr": true}
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	f.installer.Now = func() time.Time { return now }
	f.clearOutbox(t)
	episode := 0
	install := func(series int64) {
		t.Helper()
		episode++
		path := writeInstallFile(t, filepath.Join(f.root, fmt.Sprintf("Show %d S01E%02d.mkv", series, episode)), "episode")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		media := domain.Media{EntityID: 1000 + int64(episode), SeriesID: series, Title: "Show", Season: 1, Episode: episode, Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaEpisode, FileID: 2000 + int64(episode)}, Fingerprint: domain.MediaFingerprint{Path: path, FileID: 2000 + int64(episode), Size: info.Size(), ModTime: info.ModTime()}, Duration: 45 * time.Minute}
		mediaID, _, err := f.repo.UpsertMedia(f.ctx, media)
		if err != nil {
			t.Fatal(err)
		}
		source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), installSRT)
		if _, err := f.installer.Install(f.ctx, InstallRequest{MediaID: mediaID, Media: media, Language: "en", SourcePath: source, DestinationPath: strings.TrimSuffix(path, ".mkv") + ".en.srt", Candidate: domain.Candidate{ProviderID: "provider", ResultID: "candidate"}, Score: domain.Score{Total: 80}, SyncResult: domain.SyncResult{Verdict: "solid"}}); err != nil {
			t.Fatal(err)
		}
	}
	rescans := func() []int64 {
		t.Helper()
		rows, err := f.audit.Query(`SELECT next_attempt_at_ns FROM notifications WHERE notifier='arr_rescan' AND next_attempt_at_ns > 0 ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var due []int64
		for rows.Next() {
			var at int64
			if err := rows.Scan(&at); err != nil {
				t.Fatal(err)
			}
			due = append(due, at)
		}
		return due
	}
	firstDue := now.Add(notifier.ArrRescanDelay).UnixNano()
	for range 10 {
		install(5)
		now = now.Add(3 * time.Minute)
	}
	if due := rescans(); len(due) != 1 || due[0] != firstDue {
		t.Fatalf("rescans after 10 episodes = %v, want one due at %d", due, firstDue)
	}
	if n := f.count(t, `SELECT count(*) FROM notifications WHERE notifier='silo'`); n != 10 {
		t.Fatalf("silo notifications = %d, want one per change", n)
	}
	// The rescan is sent; meanwhile another episode is installed.
	leases, err := f.repo.LeaseDueNotifications(f.ctx, now, 100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	install(5)
	if due := rescans(); len(due) != 2 {
		t.Fatalf("rescans after a change in flight = %v, want a second one", due)
	}
	for _, lease := range leases {
		if err := f.repo.CompleteNotification(f.ctx, store.NotificationCompletion{JobID: lease.JobID, Result: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	if due := rescans(); len(due) != 1 || due[0] != now.Add(notifier.ArrRescanDelay).UnixNano() {
		t.Fatalf("rescans after delivery = %v, want the later change's", due)
	}
	install(6)
	install(5)
	if due := rescans(); len(due) != 2 {
		t.Fatalf("rescans with another series = %v, want one per series", due)
	}
}
