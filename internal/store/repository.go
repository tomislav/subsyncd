package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"subsyncd/internal/domain"
)

type Repository struct {
	store *Store
}

type TrackRecord struct {
	MediaID   int64
	Index     int
	Path      string
	Language  string
	Codec     string
	Embedded  bool
	Forced    bool
	Default   bool
	SDH       bool
	Protected bool
	Checksum  string
}

type InventoryRecord struct {
	Fingerprint domain.MediaFingerprint
	Tracks      []TrackRecord
}

type SearchLease struct {
	MediaID    int64
	Language   string
	JobID      string
	Attempt    int
	LeaseUntil time.Time
}

type SearchCompletion struct {
	JobID                 string
	Outcome               string
	NextAttemptAt         time.Time
	AdvanceMissingAttempt bool
}

type ProviderState struct {
	ProviderID     string
	Scope          string
	Reason         string
	Limit          int64
	Remaining      int64
	ResetAt        time.Time
	Disabled       bool
	FailureAttempt int
}

type ProviderCacheEntry struct {
	Key         string
	ProviderID  string
	ResultsJSON []byte
	ExpiresAt   time.Time
}

type PackLookup struct {
	ProviderID      string
	SeriesIDs       domain.ExternalIDs
	SeriesTitle     string
	SeriesYear      int
	Season          int
	Episode         int
	AbsoluteEpisode int
	Language        string
}

type PackCacheEntry struct {
	ID              int64
	ProviderID      string
	ResultID        string
	SeriesKey       string
	Season          int
	Language        string
	ContentChecksum string
	ManifestPath    string
	CandidateJSON   []byte
	ByteSize        int64
	ExpiresAt       time.Time
	LastAccessAt    time.Time
}

type PackMemberRecord struct {
	PackID          int64
	ProviderID      string
	ResultID        string
	Language        string
	CandidateJSON   []byte
	SafeName        string
	CachePath       string
	Checksum        string
	Season          int
	EpisodeFrom     int
	EpisodeTo       int
	AbsoluteFrom    int
	AbsoluteTo      int
	NormalizedTitle string
	Forced          bool
}

type Installation struct {
	MediaID        int64
	Language       string
	Path           string
	Checksum       string
	ProviderID     string
	CandidateID    string
	ScoreJSON      []byte
	SyncResultJSON []byte
	RollbackPath   string
}

func (r *Repository) UpsertMedia(ctx context.Context, media domain.Media) (int64, bool, error) {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin media upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id, existingFileID, existingSize, existingModTime int64
	var existingPath string
	err = tx.QueryRowContext(ctx, `SELECT id, path, file_id, size, mod_time_ns FROM media WHERE instance = ? AND kind = ? AND file_id = ?`,
		media.Ref.Instance, string(media.Ref.Kind), media.Ref.FileID).Scan(&id, &existingPath, &existingFileID, &existingSize, &existingModTime)
	changed := true
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("find media: %w", err)
	}
	if err == nil {
		changed = existingPath != media.Fingerprint.Path || existingFileID != media.Fingerprint.FileID || existingSize != media.Fingerprint.Size || existingModTime != media.Fingerprint.ModTime.UnixNano()
	}

	alternateTitles, err := json.Marshal(media.AlternateTitles)
	if err != nil {
		return 0, false, fmt.Errorf("encode alternate titles: %w", err)
	}
	now := time.Now().UTC().UnixNano()
	if id == 0 {
		result, err := tx.ExecContext(ctx, `INSERT INTO media(instance, kind, file_id, path, size, mod_time_ns, title, alternate_titles_json, year, season, episode, absolute_episode, imdb_id, tmdb_id, tvdb_id, original_filename, release_name, release_group, source, resolution, streaming_service, edition, quality, duration_ns, updated_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			media.Ref.Instance, string(media.Ref.Kind), media.Ref.FileID, media.Fingerprint.Path, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(), media.Title, alternateTitles, media.Year, media.Season, media.Episode, media.AbsoluteEpisode, media.ExternalIDs.IMDb, media.ExternalIDs.TMDB, media.ExternalIDs.TVDB, media.OriginalFilename, media.ReleaseName, media.ReleaseGroup, media.Source, media.Resolution, media.StreamingService, media.Edition, media.Quality, int64(media.Duration), now)
		if err != nil {
			return 0, false, fmt.Errorf("insert media: %w", err)
		}
		id, err = result.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("read media id: %w", err)
		}
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE media SET path=?, size=?, mod_time_ns=?, title=?, alternate_titles_json=?, year=?, season=?, episode=?, absolute_episode=?, imdb_id=?, tmdb_id=?, tvdb_id=?, original_filename=?, release_name=?, release_group=?, source=?, resolution=?, streaming_service=?, edition=?, quality=?, duration_ns=?, updated_at_ns=? WHERE id=?`,
			media.Fingerprint.Path, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(), media.Title, alternateTitles, media.Year, media.Season, media.Episode, media.AbsoluteEpisode, media.ExternalIDs.IMDb, media.ExternalIDs.TMDB, media.ExternalIDs.TVDB, media.OriginalFilename, media.ReleaseName, media.ReleaseGroup, media.Source, media.Resolution, media.StreamingService, media.Edition, media.Quality, int64(media.Duration), now, id)
		if err != nil {
			return 0, false, fmt.Errorf("update media: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit media upsert: %w", err)
	}
	return id, changed, nil
}

func (r *Repository) ReplaceTrackInventory(ctx context.Context, mediaID int64, fingerprint domain.MediaFingerprint, tracks []TrackRecord) error {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin track replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var path string
	var fileID, size, modTime int64
	if err := tx.QueryRowContext(ctx, `SELECT path, file_id, size, mod_time_ns FROM media WHERE id = ?`, mediaID).Scan(&path, &fileID, &size, &modTime); err != nil {
		return fmt.Errorf("read media fingerprint: %w", err)
	}
	if path != fingerprint.Path || fileID != fingerprint.FileID || size != fingerprint.Size || modTime != fingerprint.ModTime.UnixNano() {
		return fmt.Errorf("media fingerprint changed while replacing track inventory")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tracks WHERE media_id = ?`, mediaID); err != nil {
		return fmt.Errorf("delete old tracks: %w", err)
	}
	for _, track := range tracks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tracks(media_id, stream_index, path, language, codec, embedded, forced, is_default, sdh, protected, checksum) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			mediaID, track.Index, track.Path, track.Language, track.Codec, track.Embedded, track.Forced, track.Default, track.SDH, track.Protected, track.Checksum); err != nil {
			return fmt.Errorf("insert track: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit track replacement: %w", err)
	}
	return nil
}

func (r *Repository) GetTrackInventory(ctx context.Context, mediaID int64) (InventoryRecord, error) {
	var record InventoryRecord
	var modTime int64
	if err := r.store.db.QueryRowContext(ctx, `SELECT path, file_id, size, mod_time_ns FROM media WHERE id = ?`, mediaID).Scan(&record.Fingerprint.Path, &record.Fingerprint.FileID, &record.Fingerprint.Size, &modTime); err != nil {
		return InventoryRecord{}, fmt.Errorf("read inventory fingerprint: %w", err)
	}
	record.Fingerprint.ModTime = time.Unix(0, modTime).UTC()
	rows, err := r.store.db.QueryContext(ctx, `SELECT stream_index, path, language, codec, embedded, forced, is_default, sdh, protected, checksum FROM tracks WHERE media_id = ? ORDER BY id`, mediaID)
	if err != nil {
		return InventoryRecord{}, fmt.Errorf("list tracks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		track := TrackRecord{MediaID: mediaID}
		if err := rows.Scan(&track.Index, &track.Path, &track.Language, &track.Codec, &track.Embedded, &track.Forced, &track.Default, &track.SDH, &track.Protected, &track.Checksum); err != nil {
			return InventoryRecord{}, fmt.Errorf("scan track: %w", err)
		}
		record.Tracks = append(record.Tracks, track)
	}
	if err := rows.Err(); err != nil {
		return InventoryRecord{}, fmt.Errorf("iterate tracks: %w", err)
	}
	return record, nil
}

func (r *Repository) UpsertSearchState(ctx context.Context, mediaID int64, language domain.Language, next time.Time) error {
	_, err := r.store.db.ExecContext(ctx, `INSERT INTO search_states(media_id, language, state, attempt, failure_attempt, next_attempt_at_ns) VALUES (?, ?, 'pending', 0, 0, ?) ON CONFLICT(media_id, language) DO UPDATE SET state='pending', attempt=0, failure_attempt=0, next_attempt_at_ns=excluded.next_attempt_at_ns, lease_owner=NULL, lease_until_ns=NULL`, mediaID, language.String(), next.UnixNano())
	if err != nil {
		return fmt.Errorf("upsert search state: %w", err)
	}
	return nil
}

func (r *Repository) LeaseDueSearches(ctx context.Context, now time.Time, limit int, duration time.Duration) ([]SearchLease, error) {
	if limit <= 0 || duration <= 0 {
		return nil, fmt.Errorf("lease limit and duration must be positive")
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin search lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT media_id, language, attempt FROM search_states WHERE state = 'pending' AND next_attempt_at_ns <= ? AND (lease_until_ns IS NULL OR lease_until_ns <= ?) ORDER BY next_attempt_at_ns, media_id, language LIMIT ?`, now.UnixNano(), now.UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("select due searches: %w", err)
	}
	type due struct {
		mediaID  int64
		language string
		attempt  int
	}
	var dueRows []due
	for rows.Next() {
		var item due
		if err := rows.Scan(&item.mediaID, &item.language, &item.attempt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan due search: %w", err)
		}
		dueRows = append(dueRows, item)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close due searches: %w", err)
	}

	leaseUntil := now.Add(duration)
	leases := make([]SearchLease, 0, len(dueRows))
	for _, item := range dueRows {
		jobID, err := randomID()
		if err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE search_states SET lease_owner=?, lease_until_ns=? WHERE media_id=? AND language=? AND (lease_until_ns IS NULL OR lease_until_ns <= ?)`, jobID, leaseUntil.UnixNano(), item.mediaID, item.language, now.UnixNano())
		if err != nil {
			return nil, fmt.Errorf("claim due search: %w", err)
		}
		claimed, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("count claimed search: %w", err)
		}
		if claimed == 1 {
			leases = append(leases, SearchLease{MediaID: item.mediaID, Language: item.language, JobID: jobID, Attempt: item.attempt, LeaseUntil: leaseUntil})
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit search leases: %w", err)
	}
	return leases, nil
}

func (r *Repository) CompleteSearch(ctx context.Context, completion SearchCompletion) error {
	state := "complete"
	next := int64(0)
	if !completion.NextAttemptAt.IsZero() {
		state = "pending"
		next = completion.NextAttemptAt.UnixNano()
	}
	advance := 0
	if completion.AdvanceMissingAttempt {
		advance = 1
	}
	result, err := r.store.db.ExecContext(ctx, `UPDATE search_states SET state=?, attempt=attempt+?, next_attempt_at_ns=?, last_outcome=?, lease_owner=NULL, lease_until_ns=NULL WHERE lease_owner=?`, state, advance, next, completion.Outcome, completion.JobID)
	if err != nil {
		return fmt.Errorf("complete search: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count completed search: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("search lease %s not found", completion.JobID)
	}
	return nil
}

func (r *Repository) GetProviderState(ctx context.Context, providerID, scope string) (ProviderState, error) {
	var state ProviderState
	var reset int64
	err := r.store.db.QueryRowContext(ctx, `SELECT provider_id, scope, reason, quota_limit, quota_remaining, reset_at_ns, disabled, failure_attempt FROM provider_states WHERE provider_id=? AND scope=?`, providerID, scope).Scan(&state.ProviderID, &state.Scope, &state.Reason, &state.Limit, &state.Remaining, &reset, &state.Disabled, &state.FailureAttempt)
	if err != nil {
		return ProviderState{}, fmt.Errorf("get provider state: %w", err)
	}
	state.ResetAt = fromUnixNano(reset)
	return state, nil
}

func (r *Repository) PutProviderState(ctx context.Context, state ProviderState) error {
	_, err := r.store.db.ExecContext(ctx, `INSERT INTO provider_states(provider_id, scope, reason, quota_limit, quota_remaining, reset_at_ns, disabled, failure_attempt) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(provider_id, scope) DO UPDATE SET reason=excluded.reason, quota_limit=excluded.quota_limit, quota_remaining=excluded.quota_remaining, reset_at_ns=excluded.reset_at_ns, disabled=excluded.disabled, failure_attempt=excluded.failure_attempt`, state.ProviderID, state.Scope, state.Reason, state.Limit, state.Remaining, unixNano(state.ResetAt), state.Disabled, state.FailureAttempt)
	if err != nil {
		return fmt.Errorf("put provider state: %w", err)
	}
	return nil
}

func (r *Repository) GetProviderCache(ctx context.Context, key string, now time.Time) (ProviderCacheEntry, bool, error) {
	var entry ProviderCacheEntry
	var expires int64
	err := r.store.db.QueryRowContext(ctx, `SELECT cache_key, provider_id, results_json, expires_at_ns FROM provider_cache WHERE cache_key=? AND expires_at_ns>?`, key, now.UnixNano()).Scan(&entry.Key, &entry.ProviderID, &entry.ResultsJSON, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderCacheEntry{}, false, nil
	}
	if err != nil {
		return ProviderCacheEntry{}, false, fmt.Errorf("get provider cache: %w", err)
	}
	entry.ExpiresAt = fromUnixNano(expires)
	return entry, true, nil
}

func (r *Repository) PutProviderCache(ctx context.Context, entry ProviderCacheEntry) error {
	_, err := r.store.db.ExecContext(ctx, `INSERT INTO provider_cache(cache_key, provider_id, results_json, expires_at_ns) VALUES (?, ?, ?, ?) ON CONFLICT(cache_key) DO UPDATE SET provider_id=excluded.provider_id, results_json=excluded.results_json, expires_at_ns=excluded.expires_at_ns`, entry.Key, entry.ProviderID, entry.ResultsJSON, entry.ExpiresAt.UnixNano())
	if err != nil {
		return fmt.Errorf("put provider cache: %w", err)
	}
	return nil
}

func (r *Repository) GetReusablePackMember(ctx context.Context, lookup PackLookup, now time.Time) (PackMemberRecord, bool, error) {
	seriesKey := strongestSeriesKey(lookup.SeriesIDs, lookup.SeriesTitle, lookup.SeriesYear)
	providerClause := ""
	args := []any{seriesKey, lookup.Season, lookup.Language, now.UnixNano(), lookup.Episode, lookup.Episode, lookup.AbsoluteEpisode, lookup.AbsoluteEpisode}
	if lookup.ProviderID != "" {
		providerClause = " AND p.provider_id = ?"
		args = append(args, lookup.ProviderID)
	}
	query := `SELECT m.pack_id, p.provider_id, p.result_id, p.language, p.candidate_json, m.safe_name, m.cache_path, m.checksum, m.season, m.episode_from, m.episode_to, m.absolute_from, m.absolute_to, m.normalized_title, m.forced FROM pack_cache p JOIN pack_members m ON m.pack_id=p.id WHERE p.series_key=? AND p.season=? AND p.language=? AND p.expires_at_ns>? AND ((m.episode_from<=? AND m.episode_to>=? AND m.episode_from>0) OR (m.absolute_from<=? AND m.absolute_to>=? AND m.absolute_from>0))` + providerClause + ` ORDER BY p.last_access_at_ns DESC, m.id LIMIT 1`
	var member PackMemberRecord
	err := r.store.db.QueryRowContext(ctx, query, args...).Scan(&member.PackID, &member.ProviderID, &member.ResultID, &member.Language, &member.CandidateJSON, &member.SafeName, &member.CachePath, &member.Checksum, &member.Season, &member.EpisodeFrom, &member.EpisodeTo, &member.AbsoluteFrom, &member.AbsoluteTo, &member.NormalizedTitle, &member.Forced)
	if errors.Is(err, sql.ErrNoRows) {
		return PackMemberRecord{}, false, nil
	}
	if err != nil {
		return PackMemberRecord{}, false, fmt.Errorf("get reusable pack member: %w", err)
	}
	return member, true, nil
}

func (r *Repository) PutPack(ctx context.Context, entry PackCacheEntry, members []PackMemberRecord) error {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin put pack: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if len(entry.CandidateJSON) == 0 {
		entry.CandidateJSON = []byte(`{}`)
	}
	var packID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO pack_cache(provider_id, result_id, series_key, season, language, content_checksum, manifest_path, candidate_json, byte_size, expires_at_ns, last_access_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(provider_id, result_id, language, content_checksum) DO UPDATE SET series_key=excluded.series_key, season=excluded.season, manifest_path=excluded.manifest_path, candidate_json=excluded.candidate_json, byte_size=excluded.byte_size, expires_at_ns=excluded.expires_at_ns, last_access_at_ns=excluded.last_access_at_ns RETURNING id`, entry.ProviderID, entry.ResultID, entry.SeriesKey, entry.Season, entry.Language, entry.ContentChecksum, entry.ManifestPath, entry.CandidateJSON, entry.ByteSize, entry.ExpiresAt.UnixNano(), entry.LastAccessAt.UnixNano()).Scan(&packID)
	if err != nil {
		return fmt.Errorf("upsert pack: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pack_members WHERE pack_id=?`, packID); err != nil {
		return fmt.Errorf("replace pack members: %w", err)
	}
	for _, member := range members {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pack_members(pack_id, safe_name, cache_path, checksum, season, episode_from, episode_to, absolute_from, absolute_to, normalized_title, forced) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, packID, member.SafeName, member.CachePath, member.Checksum, member.Season, member.EpisodeFrom, member.EpisodeTo, member.AbsoluteFrom, member.AbsoluteTo, member.NormalizedTitle, member.Forced); err != nil {
			return fmt.Errorf("insert pack member: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pack: %w", err)
	}
	return nil
}

func (r *Repository) TouchPack(ctx context.Context, packID int64, now time.Time) error {
	_, err := r.store.db.ExecContext(ctx, `UPDATE pack_cache SET last_access_at_ns=? WHERE id=?`, now.UnixNano(), packID)
	if err != nil {
		return fmt.Errorf("touch pack: %w", err)
	}
	return nil
}

func (r *Repository) ListPacksForEviction(ctx context.Context, now time.Time) ([]PackCacheEntry, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT id, provider_id, result_id, series_key, season, language, content_checksum, manifest_path, candidate_json, byte_size, expires_at_ns, last_access_at_ns FROM pack_cache ORDER BY CASE WHEN expires_at_ns<=? THEN 0 ELSE 1 END, last_access_at_ns, id`, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("list packs for eviction: %w", err)
	}
	defer rows.Close()
	var entries []PackCacheEntry
	for rows.Next() {
		var entry PackCacheEntry
		var expires, lastAccess int64
		if err := rows.Scan(&entry.ID, &entry.ProviderID, &entry.ResultID, &entry.SeriesKey, &entry.Season, &entry.Language, &entry.ContentChecksum, &entry.ManifestPath, &entry.CandidateJSON, &entry.ByteSize, &expires, &lastAccess); err != nil {
			return nil, fmt.Errorf("scan pack: %w", err)
		}
		entry.ExpiresAt = fromUnixNano(expires)
		entry.LastAccessAt = fromUnixNano(lastAccess)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate packs: %w", err)
	}
	return entries, nil
}

func (r *Repository) DeletePack(ctx context.Context, packID int64) error {
	if _, err := r.store.db.ExecContext(ctx, `DELETE FROM pack_cache WHERE id=?`, packID); err != nil {
		return fmt.Errorf("delete pack: %w", err)
	}
	return nil
}

func (r *Repository) GetInstallation(ctx context.Context, mediaID int64, language domain.Language) (Installation, bool, error) {
	var installation Installation
	err := r.store.db.QueryRowContext(ctx, `SELECT media_id, language, path, checksum, provider_id, candidate_id, score_json, sync_result_json, rollback_path FROM installations WHERE media_id=? AND language=?`, mediaID, language.String()).Scan(&installation.MediaID, &installation.Language, &installation.Path, &installation.Checksum, &installation.ProviderID, &installation.CandidateID, &installation.ScoreJSON, &installation.SyncResultJSON, &installation.RollbackPath)
	if errors.Is(err, sql.ErrNoRows) {
		return Installation{}, false, nil
	}
	if err != nil {
		return Installation{}, false, fmt.Errorf("get installation: %w", err)
	}
	return installation, true, nil
}

func (r *Repository) RecordInstallation(ctx context.Context, installation Installation) error {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin installation record: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().UnixNano()
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(event_type, media_id, outcome, created_at_ns) VALUES ('subtitle_installed', ?, 'success', ?)`, installation.MediaID, now); err != nil {
		return fmt.Errorf("record installation audit: %w", err)
	}
	if len(installation.ScoreJSON) == 0 {
		installation.ScoreJSON = []byte(`{}`)
	}
	if len(installation.SyncResultJSON) == 0 {
		installation.SyncResultJSON = []byte(`{}`)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO installations(media_id, language, path, checksum, provider_id, candidate_id, score_json, sync_result_json, rollback_path, installed_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(media_id, language) DO UPDATE SET path=excluded.path, checksum=excluded.checksum, provider_id=excluded.provider_id, candidate_id=excluded.candidate_id, score_json=excluded.score_json, sync_result_json=excluded.sync_result_json, rollback_path=excluded.rollback_path, installed_at_ns=excluded.installed_at_ns`, installation.MediaID, installation.Language, installation.Path, installation.Checksum, installation.ProviderID, installation.CandidateID, installation.ScoreJSON, installation.SyncResultJSON, installation.RollbackPath, now)
	if err != nil {
		return fmt.Errorf("record installation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit installation: %w", err)
	}
	return nil
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate lease id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func unixNano(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func fromUnixNano(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func strongestSeriesKey(ids domain.ExternalIDs, title string, year int) string {
	switch {
	case ids.TVDB != 0:
		return fmt.Sprintf("tvdb:%d", ids.TVDB)
	case ids.TMDB != 0:
		return fmt.Sprintf("tmdb:%d", ids.TMDB)
	case ids.IMDb != "":
		return "imdb:" + strings.ToLower(ids.IMDb)
	default:
		return fmt.Sprintf("title:%s:%d", strings.ToLower(strings.TrimSpace(title)), year)
	}
}
