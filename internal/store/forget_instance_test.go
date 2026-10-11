package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

// seedForgetInstance indexes one episode for name with a search, an audit
// event, an unattributed event that names only its media, a candidate, a
// rejection, an installation with a rollback copy, a pending notification and
// a reconciliation replay.
func seedForgetInstance(t *testing.T, repo *Repository, name string, now time.Time) int64 {
	t.Helper()
	ctx := t.Context()
	if err := repo.EnsureInstance(ctx, name, "sonarr", "http://"+name+".invalid", now); err != nil {
		t.Fatal(err)
	}
	media := testMedia()
	media.Ref.Instance = name
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "seed-" + name, Type: "import", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"en"}, At: now}); err != nil {
		t.Fatal(err)
	}
	mediaID, _, err := repo.FindMedia(ctx, media.Ref)
	if err != nil {
		t.Fatal(err)
	}
	// Production payloads are bound as []byte and stored as BLOBs.
	payload := []byte(`{"media":{"ref":{"instance":"` + name + `","kind":"episode","file_id":42}},"subtitle_path":"/media/show.en.srt"}`)
	if _, err := repo.EnqueueNotification(ctx, NotificationRequest{Notifier: "silo", DedupeKey: "dedupe-" + name, PayloadJSON: payload, NextAttemptAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO installations(media_id,language,path,checksum,rollback_path,installed_at_ns) VALUES (?,'en','/media/show.en.srt','sum','/media/.subsyncd-rollback-1',1)`, []any{mediaID}},
		{`INSERT INTO candidates(media_id,language,provider_id,result_id,metadata_json,score_json,created_at_ns) VALUES (?,'en','p','r','{}','{}',1)`, []any{mediaID}},
		{`INSERT INTO candidate_rejections(media_id,language,provider_id,result_id,candidate_signature,reason_code,tool_signature,media_path,media_file_id,media_size,media_mod_time_ns,rejected_at_ns,expires_at_ns) VALUES (?,'en','p','r','sig','content','tool','/media/show.mkv',42,100,1,1,0)`, []any{mediaID}},
		{`INSERT INTO events(event_type,media_id,created_at_ns) VALUES (?,?,1)`, []any{"unattributed-" + name, mediaID}},
		{`INSERT INTO reconciliation_replays(event_id,instance) VALUES (?,?)`, []any{"replay-" + name, name}},
	} {
		if _, err := repo.store.db.Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return mediaID
}

func countForgetRows(t *testing.T, repo *Repository, name string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for label, query := range map[string]string{
		"instances":     `SELECT count(*) FROM instances WHERE name=?1`,
		"media":         `SELECT count(*) FROM media WHERE instance=?1`,
		"searches":      `SELECT count(*) FROM search_states JOIN media ON media.id=search_states.media_id WHERE media.instance=?1`,
		"installations": `SELECT count(*) FROM installations JOIN media ON media.id=installations.media_id WHERE media.instance=?1`,
		"candidates":    `SELECT count(*) FROM candidates JOIN media ON media.id=candidates.media_id WHERE media.instance=?1`,
		"rejections":    `SELECT count(*) FROM candidate_rejections JOIN media ON media.id=candidate_rejections.media_id WHERE media.instance=?1`,
		"events":        `SELECT count(*) FROM events WHERE instance=?1`,
		"unattributed":  `SELECT count(*) FROM events WHERE event_type='unattributed-' || ?1`,
		"notifications": `SELECT count(*) FROM notifications WHERE json_extract(payload_json,'$.media.ref.instance')=?1`,
		"replays":       `SELECT count(*) FROM reconciliation_replays WHERE instance=?1`,
	} {
		var count int
		if err := repo.store.db.QueryRow(query, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[label] = count
	}
	return counts
}

func TestForgetInstanceRemovesOnlyThatInstance(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	seedForgetInstance(t, repo, "sonarr-old", now)
	seedForgetInstance(t, repo, "sonarr-main", now)

	result, err := repo.ForgetInstance(t.Context(), "sonarr-old", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Media != 1 || result.Searches != 1 || result.Installations != 1 || result.RollbackCopies != 1 || result.Candidates != 1 || result.Rejections != 1 || result.Notifications != 1 || result.Events < 2 || result.Replays != 1 {
		t.Fatalf("result=%+v", result)
	}
	for label, count := range countForgetRows(t, repo, "sonarr-old") {
		if count != 0 {
			t.Errorf("sonarr-old %s=%d remain", label, count)
		}
	}
	for label, count := range countForgetRows(t, repo, "sonarr-main") {
		if count == 0 {
			t.Errorf("sonarr-main %s were removed", label)
		}
	}
}

func TestForgetInstancePreviewCountsWithoutDeleting(t *testing.T) {
	repo := openTestRepository(t)
	seedForgetInstance(t, repo, "sonarr-old", time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))

	result, err := repo.ForgetInstance(t.Context(), "sonarr-old", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Media != 1 || result.Installations != 1 {
		t.Fatalf("preview result=%+v", result)
	}
	for label, count := range countForgetRows(t, repo, "sonarr-old") {
		if count == 0 {
			t.Errorf("preview removed %s", label)
		}
	}
}

func TestForgetInstanceRejectsUnknownInstance(t *testing.T) {
	repo := openTestRepository(t)
	seedForgetInstance(t, repo, "sonarr-main", time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))

	if _, err := repo.ForgetInstance(t.Context(), "sonarr-typo", true); !errors.Is(err, ErrUnknownInstance) {
		t.Fatalf("err=%v", err)
	}
}

func TestForgetInstanceRollsBackOnFailure(t *testing.T) {
	repo := openTestRepository(t)
	seedForgetInstance(t, repo, "sonarr-old", time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	if _, err := repo.store.db.Exec(`CREATE TRIGGER fail_instance_delete BEFORE DELETE ON instances BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.ForgetInstance(t.Context(), "sonarr-old", true); err == nil {
		t.Fatal("expected failure")
	}
	for label, count := range countForgetRows(t, repo, "sonarr-old") {
		if count == 0 {
			t.Errorf("partial forget removed %s", label)
		}
	}
}

func TestForgetInstanceRefusesWithoutForeignKeys(t *testing.T) {
	repo := openTestRepository(t)
	seedForgetInstance(t, repo, "sonarr-old", time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	// The store keeps a single connection, so this reaches the one ForgetInstance uses.
	if _, err := repo.store.db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.ForgetInstance(t.Context(), "sonarr-old", true); err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("err=%v", err)
	}
	for label, count := range countForgetRows(t, repo, "sonarr-old") {
		if count == 0 {
			t.Errorf("forget without foreign keys removed %s", label)
		}
	}
}

func TestForgetInstanceMediaChildDeletesUseIndexes(t *testing.T) {
	repo := openTestRepository(t)
	for _, table := range []string{"candidates", "events"} {
		var plan strings.Builder
		rows, err := repo.store.db.Query(`EXPLAIN QUERY PLAN SELECT 1 FROM `+table+` WHERE media_id=?`, 1)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !strings.HasPrefix(plan.String(), "SEARCH") || !strings.Contains(plan.String(), "INDEX") {
			t.Errorf("%s media_id lookup scans: %s", table, plan.String())
		}
	}
}
