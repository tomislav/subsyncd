package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

// sortMedia is the identity that decides a search's place within a queue tie.
type sortMedia struct {
	kind            domain.MediaKind
	title           string
	seriesID        int64
	year            int
	season, episode int
}

func (m sortMedia) media(fileID int64) domain.Media {
	media := testMedia()
	media.EntityID = fileID
	media.Ref = domain.MediaRef{Instance: "sonarr", Kind: m.kind, FileID: fileID}
	media.Fingerprint.FileID = fileID
	media.Fingerprint.Path = fmt.Sprintf("/media/%d.mkv", fileID)
	media.Title, media.SeriesID, media.Year, media.Season, media.Episode = m.title, m.seriesID, m.year, m.season, m.episode
	if m.kind == domain.MediaMovie {
		media.Ref.Instance = "radarr"
	}
	return media
}

// insertSortMedia stores one episode row with the given identity and returns its ID.
func insertSortMedia(t *testing.T, repo *Repository, fileID int64, kind domain.MediaKind, title string, season, episode int) int64 {
	t.Helper()
	return insertSortItem(t, repo, fileID, sortMedia{kind: kind, title: title, seriesID: 1, season: season, episode: episode})
}

func insertSortItem(t *testing.T, repo *Repository, fileID int64, m sortMedia) int64 {
	t.Helper()
	id, _, err := repo.UpsertMedia(context.Background(), m.media(fileID))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// requireLeaseOrder queues every item at one queue position, in the given insert
// order, and requires them to lease in want order.
func requireLeaseOrder(t *testing.T, inserted, want []sortMedia) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	repo := openTestRepository(t)
	ids := map[sortMedia]int64{}
	for i, item := range inserted {
		id := insertSortItem(t, repo, int64(i+1), item)
		ids[item] = id
		requireSearchState(t, repo, id, "en", now.Add(-time.Hour), SearchPriorityMissing)
	}
	leases, err := repo.LeaseDueSearches(ctx, now, len(inserted), time.Minute)
	if err != nil || len(leases) != len(inserted) {
		t.Fatalf("leases = %d, %v", len(leases), err)
	}
	for i, w := range want {
		if leases[i].MediaID != ids[w] {
			got := make([]int64, len(leases))
			for j, lease := range leases {
				got[j] = lease.MediaID
			}
			t.Fatalf("lease %d = media %d, want %+v (media %d); full order %v", i, leases[i].MediaID, w, ids[w], got)
		}
	}
}

// Specials (season 0) follow the regular seasons of their show.
func TestLeaseDueSearchesOrdersSpecialsAfterRegularSeasons(t *testing.T) {
	ep := func(season, episode int) sortMedia {
		return sortMedia{kind: domain.MediaEpisode, title: "Battlestar Galactica (2003)", seriesID: 7, season: season, episode: episode}
	}
	requireLeaseOrder(t,
		[]sortMedia{ep(0, 2), ep(2, 1), ep(0, 1), ep(1, 1)},
		[]sortMedia{ep(1, 1), ep(2, 1), ep(0, 1), ep(0, 2)})
}

// Two shows with the same title stay apart: one runs completely before the other.
func TestLeaseDueSearchesKeepsSameTitleSeriesTogether(t *testing.T) {
	ep := func(series int64, title string, episode int) sortMedia {
		return sortMedia{kind: domain.MediaEpisode, title: title, seriesID: series, season: 1, episode: episode}
	}
	requireLeaseOrder(t,
		[]sortMedia{ep(9, "The Office", 1), ep(4, "the office", 1), ep(9, "The Office", 2), ep(4, "the office", 2)},
		[]sortMedia{ep(4, "the office", 1), ep(4, "the office", 2), ep(9, "The Office", 1), ep(9, "The Office", 2)})
}

// Movies sort by title, then year, so remakes run oldest first.
func TestLeaseDueSearchesOrdersSameTitleMoviesByYear(t *testing.T) {
	movie := func(title string, year int) sortMedia {
		return sortMedia{kind: domain.MediaMovie, title: title, year: year}
	}
	requireLeaseOrder(t,
		[]sortMedia{movie("Dune", 2021), movie("Arrival", 2016), movie("Dune", 1984)},
		[]sortMedia{movie("Arrival", 2016), movie("Dune", 1984), movie("Dune", 2021)})
}

// A batch queued at one moment (a scan or a newly configured language) shares one
// queue position; within it, searches lease by title, then season, then episode.
func TestLeaseDueSearchesOrdersQueueTiesByTitleSeasonEpisode(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	repo := openTestRepository(t)
	type item struct {
		title           string
		season, episode int
	}
	// Inserted out of order, so media_id order differs from the wanted order.
	inserted := []item{
		{"True Detective", 2, 1}, {"True Detective", 1, 1}, {"abbott Elementary", 1, 2},
		{"True Detective", 1, 10}, {"True Detective", 1, 2}, {"Abbott Elementary", 1, 1},
	}
	ids := map[item]int64{}
	for i, it := range inserted {
		id := insertSortMedia(t, repo, int64(i+1), domain.MediaEpisode, it.title, it.season, it.episode)
		ids[it] = id
		requireSearchState(t, repo, id, "en", now.Add(-time.Hour), SearchPriorityMissing)
	}
	leases, err := repo.LeaseDueSearches(ctx, now, len(inserted), time.Minute)
	if err != nil || len(leases) != len(inserted) {
		t.Fatalf("leases = %d, %v", len(leases), err)
	}
	want := []item{
		{"Abbott Elementary", 1, 1}, {"abbott Elementary", 1, 2},
		{"True Detective", 1, 1}, {"True Detective", 1, 2}, {"True Detective", 1, 10}, {"True Detective", 2, 1},
	}
	for i, w := range want {
		if leases[i].MediaID != ids[w] {
			got := make([]int64, len(leases))
			for j, lease := range leases {
				got[j] = lease.MediaID
			}
			t.Fatalf("lease %d = media %d, want %v (media %d); full order %v", i, leases[i].MediaID, w, ids[w], got)
		}
	}
}

// The media order is only a tie-breaker: class, instance rank and queue position
// still come first.
func TestLeaseDueSearchesKeepsQueuePositionBeforeMediaOrder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	repo := openTestRepository(t)
	later := insertSortMedia(t, repo, 1, domain.MediaEpisode, "Abbott Elementary", 1, 1)
	earlier := insertSortMedia(t, repo, 2, domain.MediaEpisode, "Zorro", 1, 1)
	requireSearchState(t, repo, later, "en", now.Add(-time.Minute), SearchPriorityMissing)
	requireSearchState(t, repo, earlier, "en", now.Add(-time.Hour), SearchPriorityMissing)
	leases, err := repo.LeaseDueSearches(ctx, now, 2, time.Minute)
	if err != nil || len(leases) != 2 {
		t.Fatalf("leases = %d, %v", len(leases), err)
	}
	if leases[0].MediaID != earlier || leases[1].MediaID != later {
		t.Fatalf("lease order = %d,%d, want older queue position first (%d,%d)", leases[0].MediaID, leases[1].MediaID, earlier, later)
	}
}

// The copied sort key follows the media row when its title, season or episode changes.
func TestSearchMediaOrderFollowsMediaChanges(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	repo := openTestRepository(t)
	item := sortMedia{kind: domain.MediaEpisode, title: "Show", seriesID: 1, season: 1, episode: 2}
	id := insertSortItem(t, repo, 1, item)
	requireSearchState(t, repo, id, "en", now, SearchPriorityMissing)
	if _, err := repo.EnsureConfiguredLanguageSearches(ctx, []string{"sonarr"}, []domain.Language{"hr"}, now); err != nil {
		t.Fatal(err)
	}
	sortKey := func(language string) string {
		t.Helper()
		var title string
		var series, season, episode int64
		if err := repo.store.db.QueryRow(`SELECT sort_title, sort_series, sort_season, sort_episode FROM search_states WHERE media_id=? AND language=?`, id, language).Scan(&title, &series, &season, &episode); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s/%d/%d/%d", title, series, season, episode)
	}
	for _, language := range []string{"en", "hr"} {
		if got := sortKey(language); got != "show/1/1/2" {
			t.Fatalf("%s sort key = %s, want show/1/1/2", language, got)
		}
	}
	// The Arr renames the series and renumbers the file; the repository's own
	// upsert path must carry that into the queued searches.
	item.title, item.season, item.episode = "The Show", 3, 4
	if _, _, err := repo.UpsertMedia(ctx, item.media(1)); err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{"en", "hr"} {
		if got := sortKey(language); got != "the show/1/3/4" {
			t.Fatalf("%s sort key after upsert = %s, want the show/1/3/4", language, got)
		}
	}
}

// Migration 017 backfills the sort key on existing searches and serves it from
// the lease index.
func TestMigrationBackfillsSearchMediaOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "subsyncd.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, err := old.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.Name() < "017_" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		contents, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(string(contents)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := old.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO media(id,instance,kind,entity_id,file_id,path,size,mod_time_ns,title,year,series_id,season,episode,updated_at_ns) VALUES (1,'radarr','movie',1,11,'/media/movie.mkv',100,1,'Movie',1999,0,0,0,1),(2,'sonarr','episode',2,22,'/media/episode.mkv',200,2,'Show',2008,5,2,5,2),(3,'sonarr','episode',3,33,'/media/special.mkv',300,3,'Show',2008,5,0,1,3)`,
		`INSERT INTO search_states(media_id,language,state,next_attempt_at_ns,priority) VALUES (1,'en','pending',500,200),(2,'hr','pending',300,100),(3,'en','pending',300,100)`,
	} {
		if _, err := old.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	got := map[int64]string{}
	rows, err := migrated.db.Query(`SELECT media_id, sort_title, sort_series, sort_season, sort_episode FROM search_states`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var title string
		var series, season, episode int64
		if err := rows.Scan(&id, &title, &series, &season, &episode); err != nil {
			t.Fatal(err)
		}
		got[id] = fmt.Sprintf("%s/%d/%d/%d", title, series, season, episode)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Movies order by year; specials (season 0) sort after every regular season.
	if got[1] != "movie/0/1999/0" || got[2] != "show/5/2/5" || got[3] != "show/5/1000000/1" {
		t.Fatalf("backfilled sort keys = %v", got)
	}
	var index string
	if err := migrated.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name='search_due_idx'`).Scan(&index); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(index, "sort_title") || !strings.Contains(index, "sort_episode") || !strings.Contains(index, "queue_order_ns") {
		t.Fatalf("search_due_idx = %q, want the queue position and media order columns", index)
	}
	// One insert trigger fills both the instance rank and the media order.
	var triggers []string
	trows, err := migrated.db.Query(`SELECT name FROM sqlite_master WHERE type='trigger' AND tbl_name='search_states' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer trows.Close()
	for trows.Next() {
		var name string
		if err := trows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		triggers = append(triggers, name)
	}
	if strings.Join(triggers, ",") != "search_states_queue_order" {
		t.Fatalf("search_states triggers = %v, want only search_states_queue_order", triggers)
	}
}
