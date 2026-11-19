package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

// seedMovieToMove indexes the movie "Dune" (TMDB 438631, file 7, entity 3)
// under radarr at /media/movies/Dune (2021)/Dune.mkv with an English
// installation and rollback copy, an external track, a hash, a rejection and
// a LAPSE failure recorded for that file, a search and a history row.
func seedMovieToMove(t *testing.T, repo *Repository) int64 {
	t.Helper()
	ctx := t.Context()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"radarr", "radarr-1080p"} {
		if err := repo.EnsureInstance(ctx, name, "radarr", "http://"+name+".invalid", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.SetInstanceQueuePriorities(ctx, map[string]int{"radarr-1080p": 5}); err != nil {
		t.Fatal(err)
	}
	media := domain.Media{
		EntityID: 3, Ref: domain.MediaRef{Instance: "radarr", Kind: domain.MediaMovie, FileID: 7},
		Fingerprint: domain.MediaFingerprint{Path: "/media/movies/Dune (2021)/Dune.mkv", FileID: 7, Size: 100, ModTime: now},
		Title:       "Dune", Year: 2021, ExternalIDs: domain.ExternalIDs{TMDB: 438631},
	}
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "seed-dune", Type: "import", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"en"}, At: now}); err != nil {
		t.Fatal(err)
	}
	mediaID, _, err := repo.FindMedia(ctx, media.Ref)
	if err != nil {
		t.Fatal(err)
	}
	dir := "/media/movies/Dune (2021)/"
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO installations(media_id,language,path,checksum,rollback_path,installed_at_ns,media_path,media_file_id,media_size,media_mod_time_ns) VALUES (?1,'en',?2,'sum',?3,1,?4,7,100,?5)`, []any{mediaID, dir + "Dune.en.srt", dir + ".subsyncd-rollback-1", dir + "Dune.mkv", now.UnixNano()}},
		{`INSERT INTO tracks(media_id,path,language,embedded,forced,is_default,sdh,protected) VALUES (?,?,'en',0,0,0,0,1)`, []any{mediaID, dir + "Dune.en.srt"}},
		{`INSERT INTO tracks(media_id,stream_index,language,embedded,forced,is_default,sdh,protected) VALUES (?,2,'hr',1,0,0,0,1)`, []any{mediaID}},
		{`INSERT INTO media_hashes(media_id,algorithm,hash_value,byte_size,fingerprint_path,fingerprint_file_id,fingerprint_size,fingerprint_mod_time_ns,updated_at_ns) VALUES (?,'oshash','h',100,?,7,100,1,1)`, []any{mediaID, dir + "Dune.mkv"}},
		{`INSERT INTO candidate_rejections(media_id,language,provider_id,result_id,candidate_signature,reason_code,tool_signature,media_path,media_file_id,media_size,media_mod_time_ns,rejected_at_ns,expires_at_ns) VALUES (?,'en','p','r','sig','user','tool',?,7,100,1,1,0)`, []any{mediaID, dir + "Dune.mkv"}},
		{`INSERT INTO candidate_lapse_failures(media_id,language,provider_id,result_id,candidate_signature,artifact_checksum,tool_signature,media_path,media_file_id,media_size,media_mod_time_ns,failure_signature,occurrence_count,first_observed_at_ns,last_observed_at_ns) VALUES (?,'en','p','r','sig','a','tool',?,7,100,1,'f',1,1,1)`, []any{mediaID, dir + "Dune.mkv"}},
		{`INSERT INTO inventory_probes(media_id,path,file_id,size,mod_time_ns) VALUES (?,?,7,100,1)`, []any{mediaID, dir + "Dune.mkv"}},
		{`INSERT INTO search_history(media_id,instance,kind,file_id,media_title,language,outcome,finished_at_ns) VALUES (?,'radarr','movie',7,'Dune','en','installed',1)`, []any{mediaID}},
	} {
		if _, err := repo.store.db.Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return mediaID
}

func duneMove() MediaMoveEntry {
	return MediaMoveEntry{Kind: domain.MediaMovie, TMDBID: 438631, ToEntityID: 30, ToFileID: 70, ToPath: "/media/movies-1080p/Dune (2021)/Dune.mkv"}
}

func queryString(t *testing.T, repo *Repository, query string, args ...any) string {
	t.Helper()
	var value string
	if err := repo.store.db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return value
}

func TestMoveMediaMovesRowInPlace(t *testing.T) {
	repo := openTestRepository(t)
	mediaID := seedMovieToMove(t, repo)
	checked := 0
	items, err := repo.MoveMedia(t.Context(), "radarr", "radarr-1080p", []MediaMoveEntry{duneMove()}, func(_ context.Context, item *MediaMoveItem) { checked++ }, true)
	if err != nil {
		t.Fatal(err)
	}
	if checked != 1 || len(items) != 1 || items[0].MediaID != mediaID || len(items[0].Sidecars) != 1 || items[0].Tracks != 1 {
		t.Fatalf("items = %#v, checked %d", items, checked)
	}
	sidecar := items[0].Sidecars[0]
	if sidecar.To != "/media/movies-1080p/Dune (2021)/Dune.en.srt" || sidecar.RollbackTo != "/media/movies-1080p/Dune (2021)/.subsyncd-rollback-1" {
		t.Fatalf("sidecar = %#v", sidecar)
	}
	dir := "/media/movies-1080p/Dune (2021)/"
	for query, want := range map[string]string{
		`SELECT instance || '|' || entity_id || '|' || file_id || '|' || path FROM media WHERE id=?`:                                                "radarr-1080p|30|70|" + dir + "Dune.mkv",
		`SELECT path || '|' || rollback_path || '|' || media_path || '|' || media_file_id || '|' || media_size FROM installations WHERE media_id=?`: dir + "Dune.en.srt|" + dir + ".subsyncd-rollback-1|" + dir + "Dune.mkv|70|100",
		`SELECT path FROM tracks WHERE media_id=? AND embedded=0`:                                                                                   dir + "Dune.en.srt",
		`SELECT path FROM tracks WHERE media_id=? AND embedded=1`:                                                                                   "",
		`SELECT fingerprint_path || '|' || fingerprint_file_id FROM media_hashes WHERE media_id=?`:                                                  dir + "Dune.mkv|70",
		`SELECT media_path || '|' || media_file_id FROM candidate_rejections WHERE media_id=?`:                                                      dir + "Dune.mkv|70",
		`SELECT media_path || '|' || media_file_id FROM candidate_lapse_failures WHERE media_id=?`:                                                  dir + "Dune.mkv|70",
		`SELECT count(*) FROM inventory_probes WHERE media_id=?`:                                                                                    "0",
		`SELECT instance_rank FROM search_states WHERE media_id=?`:                                                                                  "5",
		`SELECT instance FROM search_history WHERE media_id=?`:                                                                                      "radarr",
	} {
		got := queryString(t, repo, query, mediaID)
		if got != want {
			t.Errorf("%s = %q, want %q", query, got, want)
		}
	}
}

func TestMoveMediaPreviewWritesNothing(t *testing.T) {
	repo := openTestRepository(t)
	mediaID := seedMovieToMove(t, repo)
	if _, err := repo.MoveMedia(t.Context(), "radarr", "radarr-1080p", []MediaMoveEntry{duneMove()}, nil, false); err != nil {
		t.Fatal(err)
	}
	if got := queryString(t, repo, `SELECT instance || '|' || file_id FROM media WHERE id=?`, mediaID); got != "radarr|7" {
		t.Fatalf("media = %q after preview", got)
	}
}

func TestMoveMediaRefusesWholePlanOnAnyProblem(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, repo *Repository, mediaID int64)
		entries func() []MediaMoveEntry
		check   MediaMoveCheck
		want    string
	}{
		{"no match", nil, func() []MediaMoveEntry { e := duneMove(); e.TMDBID = 1; return []MediaMoveEntry{e} }, nil, "no active movie under radarr matches"},
		{"from_file_id disagrees", nil, func() []MediaMoveEntry { e := duneMove(); e.FromFileID = 8; return []MediaMoveEntry{e} }, nil, "no active movie under radarr matches"},
		{"conflict under to", func(t *testing.T, repo *Repository, _ int64) {
			other := domain.Media{EntityID: 30, Ref: domain.MediaRef{Instance: "radarr-1080p", Kind: domain.MediaMovie, FileID: 99}, Fingerprint: domain.MediaFingerprint{Path: "/media/movies-1080p/x.mkv", FileID: 99, Size: 1, ModTime: time.Unix(1, 0)}, Title: "X"}
			if _, _, err := repo.UpsertMedia(ctx, other); err != nil {
				t.Fatal(err)
			}
		}, func() []MediaMoveEntry { return []MediaMoveEntry{duneMove()} }, nil, "radarr-1080p already has a movie row"},
		{"duplicate entry", nil, func() []MediaMoveEntry {
			e := duneMove()
			f := duneMove()
			f.ToFileID, f.ToEntityID = 71, 31
			return []MediaMoveEntry{e, f}
		}, nil, "matches the same media as entry 1"},
		{"pending install", func(t *testing.T, repo *Repository, mediaID int64) {
			if _, err := repo.store.db.Exec(`INSERT INTO pending_installs(media_id,language,destination,checksum,created_at_ns) VALUES (?,'en','/x','s',1)`, mediaID); err != nil {
				t.Fatal(err)
			}
		}, func() []MediaMoveEntry { return []MediaMoveEntry{duneMove()} }, nil, "interrupted install"},
		{"installation for an earlier file", func(t *testing.T, repo *Repository, mediaID int64) {
			if _, err := repo.store.db.Exec(`UPDATE installations SET media_file_id=6 WHERE media_id=?`, mediaID); err != nil {
				t.Fatal(err)
			}
		}, func() []MediaMoveEntry { return []MediaMoveEntry{duneMove()} }, nil, "recorded for an earlier file"},
		{"episode fields on a movie", nil, func() []MediaMoveEntry { e := duneMove(); e.Season = 1; return []MediaMoveEntry{e} }, nil, "a movie takes no"},
		{"relative path", nil, func() []MediaMoveEntry { e := duneMove(); e.ToPath = "Dune.mkv"; return []MediaMoveEntry{e} }, nil, "clean absolute path"},
		{"file check", nil, func() []MediaMoveEntry { return []MediaMoveEntry{duneMove()} }, func(_ context.Context, item *MediaMoveItem) {
			item.Problems = append(item.Problems, "sidecar missing")
		}, "sidecar missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := openTestRepository(t)
			mediaID := seedMovieToMove(t, repo)
			if test.prepare != nil {
				test.prepare(t, repo, mediaID)
			}
			items, err := repo.MoveMedia(ctx, "radarr", "radarr-1080p", test.entries(), test.check, true)
			if !errors.Is(err, ErrMediaMoveRefused) {
				t.Fatalf("err = %v, want refused", err)
			}
			var problems []string
			for _, item := range items {
				problems = append(problems, item.Problems...)
			}
			if !strings.Contains(strings.Join(problems, "; "), test.want) {
				t.Fatalf("problems = %q, want %q", problems, test.want)
			}
			if got := queryString(t, repo, `SELECT instance || '|' || file_id FROM media WHERE id=?`, mediaID); got != "radarr|7" {
				t.Fatalf("media = %q after a refused move", got)
			}
			if got := queryString(t, repo, `SELECT path FROM installations WHERE media_id=?`, mediaID); got != "/media/movies/Dune (2021)/Dune.en.srt" {
				t.Fatalf("installation = %q after a refused move", got)
			}
		})
	}
}

func TestMoveMediaMatchesEpisodesByIdentityOrFileID(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	media := testMedia() // sonarr-main episode S01E02 of TVDB 1, file 42
	if _, _, err := repo.UpsertMedia(ctx, media); err != nil {
		t.Fatal(err)
	}
	move := MediaMoveEntry{Kind: domain.MediaEpisode, TVDBID: 1, Season: 1, Episode: 2, ToEntityID: 500, ToFileID: 600, ToPath: "/media/tv-1080p/show.mkv"}
	if _, err := repo.MoveMedia(ctx, "sonarr-main", "sonarr-1080p", []MediaMoveEntry{move}, nil, true); !errors.Is(err, ErrMediaMoveRefused) {
		t.Fatalf("err = %v, want refused for a missing to_series_id", err)
	}
	move.ToSeriesID = 50
	if _, err := repo.MoveMedia(ctx, "sonarr-main", "sonarr-1080p", []MediaMoveEntry{move}, nil, true); err != nil {
		t.Fatal(err)
	}
	if got := queryString(t, repo, `SELECT instance || '|' || series_id || '|' || entity_id || '|' || file_id FROM media WHERE path=?`, move.ToPath); got != "sonarr-1080p|50|500|600" {
		t.Fatalf("moved episode = %q", got)
	}
	back := MediaMoveEntry{Kind: domain.MediaEpisode, FromFileID: 600, ToSeriesID: 5, ToEntityID: 101, ToFileID: 42, ToPath: "/media/show.mkv"}
	if _, err := repo.MoveMedia(ctx, "sonarr-1080p", "sonarr-main", []MediaMoveEntry{back}, nil, true); err != nil {
		t.Fatal(err)
	}
}

func TestMoveMediaRefusesWithoutForeignKeys(t *testing.T) {
	repo := openTestRepository(t)
	seedMovieToMove(t, repo)
	if _, err := repo.store.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = repo.store.db.Exec(`PRAGMA foreign_keys=ON`) })
	if _, err := repo.MoveMedia(t.Context(), "radarr", "radarr-1080p", []MediaMoveEntry{duneMove()}, nil, true); err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("err = %v, want foreign key refusal", err)
	}
}

func TestMoveMediaNamesSidecarsAfterARenamedMediaFile(t *testing.T) {
	repo := openTestRepository(t)
	seedMovieToMove(t, repo)
	move := duneMove()
	move.ToPath = "/media/movies-1080p/Dune (2021)/Dune (2021) Bluray-1080p.mkv"
	items, err := repo.MoveMedia(t.Context(), "radarr", "radarr-1080p", []MediaMoveEntry{move}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	sidecar := items[0].Sidecars[0]
	if sidecar.To != "/media/movies-1080p/Dune (2021)/Dune (2021) Bluray-1080p.en.srt" || sidecar.RollbackTo != "/media/movies-1080p/Dune (2021)/.subsyncd-rollback-1" {
		t.Fatalf("sidecar = %#v, want it named after the new media file", sidecar)
	}
}

func TestMoveMediaRefusesWhileWebhooksWait(t *testing.T) {
	for _, instance := range []string{"radarr", "radarr-1080p"} {
		t.Run(instance, func(t *testing.T) {
			repo := openTestRepository(t)
			mediaID := seedMovieToMove(t, repo)
			if _, err := repo.EnqueueWebhook(t.Context(), instance, []byte(`{}`), time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.MoveMedia(t.Context(), "radarr", "radarr-1080p", []MediaMoveEntry{duneMove()}, nil, true); !errors.Is(err, ErrMediaMoveWebhooksWaiting) {
				t.Fatalf("err = %v, want waiting webhooks", err)
			}
			if got := queryString(t, repo, `SELECT instance FROM media WHERE id=?`, mediaID); got != "radarr" {
				t.Fatalf("media instance = %q", got)
			}
		})
	}
}
