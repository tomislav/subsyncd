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

// MediaEventMutation is the persistence-level representation of an Arr event.
// Event IDs are globally unique so replayed webhooks remain no-ops.
type MediaEventMutation struct {
	EventID   string
	Type      string
	Media     domain.Media
	Ref       domain.MediaRef
	Languages []domain.Language
	At        time.Time
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
		result, err := tx.ExecContext(ctx, `INSERT INTO media(instance, kind, file_id, path, size, mod_time_ns, title, episode_title, alternate_titles_json, year, season, episode, absolute_episode, imdb_id, tmdb_id, tvdb_id, original_filename, release_name, release_group, source, resolution, streaming_service, edition, quality, duration_ns, updated_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			media.Ref.Instance, string(media.Ref.Kind), media.Ref.FileID, media.Fingerprint.Path, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(), media.Title, media.EpisodeTitle, alternateTitles, media.Year, media.Season, media.Episode, media.AbsoluteEpisode, media.ExternalIDs.IMDb, media.ExternalIDs.TMDB, media.ExternalIDs.TVDB, media.OriginalFilename, media.ReleaseName, media.ReleaseGroup, media.Source, media.Resolution, media.StreamingService, media.Edition, media.Quality, int64(media.Duration), now)
		if err != nil {
			return 0, false, fmt.Errorf("insert media: %w", err)
		}
		id, err = result.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("read media id: %w", err)
		}
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE media SET path=?, size=?, mod_time_ns=?, title=?, episode_title=?, alternate_titles_json=?, year=?, season=?, episode=?, absolute_episode=?, imdb_id=?, tmdb_id=?, tvdb_id=?, original_filename=?, release_name=?, release_group=?, source=?, resolution=?, streaming_service=?, edition=?, quality=?, duration_ns=?, updated_at_ns=? WHERE id=?`,
			media.Fingerprint.Path, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(), media.Title, media.EpisodeTitle, alternateTitles, media.Year, media.Season, media.Episode, media.AbsoluteEpisode, media.ExternalIDs.IMDb, media.ExternalIDs.TMDB, media.ExternalIDs.TVDB, media.OriginalFilename, media.ReleaseName, media.ReleaseGroup, media.Source, media.Resolution, media.StreamingService, media.Edition, media.Quality, int64(media.Duration), now, id)
		if err != nil {
			return 0, false, fmt.Errorf("update media: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit media upsert: %w", err)
	}
	return id, changed, nil
}

// GetMediaHash returns a cached content hash only when it was calculated for
// the exact media fingerprint supplied by the caller. A changed path, Arr file
// ID, size, or modification time is therefore a cache miss.
func (r *Repository) GetMediaHash(ctx context.Context, media domain.Media, algorithm string) (string, int64, bool, error) {
	var value string
	var byteSize int64
	err := r.store.db.QueryRowContext(ctx, `
		SELECT h.hash_value, h.byte_size
		FROM media_hashes h
		JOIN media m ON m.id = h.media_id
		WHERE m.instance = ? AND m.kind = ? AND m.file_id = ?
		  AND m.path = ?
		  AND m.size = ?
		  AND m.mod_time_ns = ?
		  AND h.algorithm = ?
		  AND h.fingerprint_path = ?
		  AND h.fingerprint_file_id = ?
		  AND h.fingerprint_size = ?
		  AND h.fingerprint_mod_time_ns = ?`,
		media.Ref.Instance, string(media.Ref.Kind), media.Ref.FileID,
		media.Fingerprint.Path, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(), algorithm,
		media.Fingerprint.Path, media.Fingerprint.FileID, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(),
	).Scan(&value, &byteSize)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("get %s media hash: %w", algorithm, err)
	}
	return value, byteSize, true, nil
}

// PutMediaHash stores a content hash only if the database still describes the
// exact file that was hashed. This prevents a concurrent rescan or replacement
// from attaching a stale hash to new media bytes.
func (r *Repository) PutMediaHash(ctx context.Context, media domain.Media, algorithm, value string, byteSize int64) error {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s media hash write: %w", algorithm, err)
	}
	defer func() { _ = tx.Rollback() }()

	var mediaID int64
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM media
		WHERE instance = ? AND kind = ? AND file_id = ?
		  AND path = ? AND size = ? AND mod_time_ns = ?`,
		media.Ref.Instance, string(media.Ref.Kind), media.Ref.FileID,
		media.Fingerprint.Path, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(),
	).Scan(&mediaID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store %s media hash: media fingerprint changed", algorithm)
	}
	if err != nil {
		return fmt.Errorf("find media for %s hash: %w", algorithm, err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO media_hashes(media_id, algorithm, hash_value, byte_size, fingerprint_path, fingerprint_file_id, fingerprint_size, fingerprint_mod_time_ns, updated_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(media_id, algorithm) DO UPDATE SET
		  hash_value=excluded.hash_value,
		  byte_size=excluded.byte_size,
		  fingerprint_path=excluded.fingerprint_path,
		  fingerprint_file_id=excluded.fingerprint_file_id,
		  fingerprint_size=excluded.fingerprint_size,
		  fingerprint_mod_time_ns=excluded.fingerprint_mod_time_ns,
		  updated_at_ns=excluded.updated_at_ns`,
		mediaID, algorithm, value, byteSize, media.Fingerprint.Path, media.Fingerprint.FileID,
		media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(), time.Now().UTC().UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("store %s media hash: %w", algorithm, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s media hash: %w", algorithm, err)
	}
	return nil
}

func (r *Repository) ReplaceTrackInventory(ctx context.Context, mediaID int64, fingerprint domain.MediaFingerprint, tracks []TrackRecord) error {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin track replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE media SET path=?, file_id=?, size=?, mod_time_ns=?, updated_at_ns=? WHERE id=?`, fingerprint.Path, fingerprint.FileID, fingerprint.Size, fingerprint.ModTime.UnixNano(), time.Now().UTC().UnixNano(), mediaID)
	if err != nil {
		return fmt.Errorf("update inventory fingerprint: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count inventory fingerprint update: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("media %d not found while replacing track inventory", mediaID)
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
	seriesKeys := allSeriesKeys(lookup.SeriesIDs, lookup.SeriesTitle, lookup.SeriesYear)
	providerClause := ""
	args := make([]any, 0, len(seriesKeys)+8)
	for _, key := range seriesKeys {
		args = append(args, key)
	}
	args = append(args, lookup.Season, lookup.Language, now.UnixNano(), lookup.Episode, lookup.Episode, lookup.AbsoluteEpisode, lookup.AbsoluteEpisode)
	if lookup.ProviderID != "" {
		providerClause = " AND p.provider_id = ?"
		args = append(args, lookup.ProviderID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(seriesKeys)), ",")
	query := `SELECT m.pack_id, p.provider_id, p.result_id, p.language, p.candidate_json, m.safe_name, m.cache_path, m.checksum, m.season, m.episode_from, m.episode_to, m.absolute_from, m.absolute_to, m.normalized_title, m.forced FROM pack_cache p JOIN pack_members m ON m.pack_id=p.id WHERE p.series_key IN (` + placeholders + `) AND p.season=? AND p.language=? AND p.expires_at_ns>? AND m.forced=0 AND ((m.episode_from<=? AND m.episode_to>=? AND m.episode_from>0) OR (m.absolute_from<=? AND m.absolute_to>=? AND m.absolute_from>0))` + providerClause + ` ORDER BY p.last_access_at_ns DESC, m.id LIMIT 1`
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

func (r *Repository) ApplyMediaEvent(ctx context.Context, mutation MediaEventMutation) (bool, error) {
	if mutation.EventID == "" || mutation.Ref.Instance == "" || mutation.Ref.FileID <= 0 {
		return false, fmt.Errorf("media event identity is incomplete")
	}
	if mutation.At.IsZero() {
		mutation.At = time.Now().UTC()
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin media event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT INTO events(event_id, event_type, instance, kind, file_id, outcome, created_at_ns) VALUES (?, ?, ?, ?, ?, 'applied', ?) ON CONFLICT DO NOTHING`, mutation.EventID, mutation.Type, mutation.Ref.Instance, mutation.Ref.Kind, mutation.Ref.FileID, mutation.At.UnixNano())
	if err != nil {
		return false, fmt.Errorf("record media event: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count media event insertion: %w", err)
	}
	if inserted == 0 {
		return false, nil
	}

	switch mutation.Type {
	case "import", "rename":
		mediaID, contentChanged, err := upsertMediaTx(ctx, tx, mutation.Media, mutation.At)
		if err != nil {
			return false, err
		}
		if contentChanged {
			if _, err := tx.ExecContext(ctx, `DELETE FROM candidates WHERE media_id=?`, mediaID); err != nil {
				return false, fmt.Errorf("invalidate media candidates: %w", err)
			}
		}
		for _, language := range mutation.Languages {
			if _, err := tx.ExecContext(ctx, `INSERT INTO search_states(media_id, language, state, attempt, failure_attempt, next_attempt_at_ns) VALUES (?, ?, 'pending', 0, 0, ?) ON CONFLICT(media_id, language) DO UPDATE SET state='pending', attempt=0, failure_attempt=0, next_attempt_at_ns=excluded.next_attempt_at_ns, last_outcome='', lease_owner=NULL, lease_until_ns=NULL`, mediaID, language.String(), mutation.At.UnixNano()); err != nil {
				return false, fmt.Errorf("reset media search: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE events SET media_id=? WHERE event_id=?`, mediaID, mutation.EventID); err != nil {
			return false, fmt.Errorf("link media event: %w", err)
		}
	case "delete":
		var mediaID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM media WHERE instance=? AND kind=? AND file_id=?`, mutation.Ref.Instance, mutation.Ref.Kind, mutation.Ref.FileID).Scan(&mediaID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("find deleted media: %w", err)
		}
		if err == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE search_states SET state='complete', last_outcome='deleted', lease_owner=NULL, lease_until_ns=NULL WHERE media_id=?`, mediaID); err != nil {
				return false, fmt.Errorf("cancel deleted media searches: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE events SET media_id=? WHERE event_id=?`, mediaID, mutation.EventID); err != nil {
				return false, fmt.Errorf("link delete event: %w", err)
			}
		}
	default:
		return false, fmt.Errorf("unsupported media event type %q", mutation.Type)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id IN (SELECT id FROM events ORDER BY created_at_ns DESC, id DESC LIMIT -1 OFFSET 10000)`); err != nil {
		return false, fmt.Errorf("bound media audit log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit media event: %w", err)
	}
	return true, nil
}

func upsertMediaTx(ctx context.Context, tx *sql.Tx, media domain.Media, at time.Time) (int64, bool, error) {
	var id, existingFileID, existingSize, existingModTime int64
	var existingPath string
	err := tx.QueryRowContext(ctx, `SELECT id, path, file_id, size, mod_time_ns FROM media WHERE instance=? AND kind=? AND file_id=?`, media.Ref.Instance, media.Ref.Kind, media.Ref.FileID).Scan(&id, &existingPath, &existingFileID, &existingSize, &existingModTime)
	changed := true
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("find media for event: %w", err)
	}
	if err == nil {
		changed = existingFileID != media.Fingerprint.FileID || existingSize != media.Fingerprint.Size || existingModTime != media.Fingerprint.ModTime.UnixNano()
	}
	alternateTitles, err := json.Marshal(media.AlternateTitles)
	if err != nil {
		return 0, false, fmt.Errorf("encode media alternate titles: %w", err)
	}
	if id == 0 {
		result, err := tx.ExecContext(ctx, `INSERT INTO media(instance, kind, file_id, path, size, mod_time_ns, title, episode_title, alternate_titles_json, year, season, episode, absolute_episode, imdb_id, tmdb_id, tvdb_id, original_filename, release_name, release_group, source, resolution, streaming_service, edition, quality, duration_ns, updated_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, media.Ref.Instance, media.Ref.Kind, media.Ref.FileID, media.Fingerprint.Path, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(), media.Title, media.EpisodeTitle, alternateTitles, media.Year, media.Season, media.Episode, media.AbsoluteEpisode, media.ExternalIDs.IMDb, media.ExternalIDs.TMDB, media.ExternalIDs.TVDB, media.OriginalFilename, media.ReleaseName, media.ReleaseGroup, media.Source, media.Resolution, media.StreamingService, media.Edition, media.Quality, int64(media.Duration), at.UnixNano())
		if err != nil {
			return 0, false, fmt.Errorf("insert media for event: %w", err)
		}
		id, err = result.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("read media ID for event: %w", err)
		}
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE media SET path=?, size=?, mod_time_ns=?, title=?, episode_title=?, alternate_titles_json=?, year=?, season=?, episode=?, absolute_episode=?, imdb_id=?, tmdb_id=?, tvdb_id=?, original_filename=?, release_name=?, release_group=?, source=?, resolution=?, streaming_service=?, edition=?, quality=?, duration_ns=?, updated_at_ns=? WHERE id=?`, media.Fingerprint.Path, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(), media.Title, media.EpisodeTitle, alternateTitles, media.Year, media.Season, media.Episode, media.AbsoluteEpisode, media.ExternalIDs.IMDb, media.ExternalIDs.TMDB, media.ExternalIDs.TVDB, media.OriginalFilename, media.ReleaseName, media.ReleaseGroup, media.Source, media.Resolution, media.StreamingService, media.Edition, media.Quality, int64(media.Duration), at.UnixNano(), id)
		if err != nil {
			return 0, false, fmt.Errorf("update media for event: %w", err)
		}
	}
	_ = existingPath
	return id, changed, nil
}

func (r *Repository) EnsureInstance(ctx context.Context, name, instanceType, baseURL string, now time.Time) error {
	_, err := r.store.db.ExecContext(ctx, `INSERT INTO instances(name, type, base_url, updated_at_ns) VALUES (?, ?, ?, ?) ON CONFLICT(name) DO UPDATE SET type=excluded.type, base_url=excluded.base_url, updated_at_ns=excluded.updated_at_ns`, name, instanceType, baseURL, now.UnixNano())
	if err != nil {
		return fmt.Errorf("ensure Arr instance: %w", err)
	}
	return nil
}

func (r *Repository) GetReconciliationCursor(ctx context.Context, instance string) (time.Time, error) {
	var raw string
	if err := r.store.db.QueryRowContext(ctx, `SELECT reconciliation_cursor FROM instances WHERE name=?`, instance).Scan(&raw); err != nil {
		return time.Time{}, fmt.Errorf("get reconciliation cursor: %w", err)
	}
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse reconciliation cursor: %w", err)
	}
	return parsed, nil
}

func (r *Repository) CommitReconciliationCursor(ctx context.Context, instance string, cursor time.Time) error {
	result, err := r.store.db.ExecContext(ctx, `UPDATE instances SET reconciliation_cursor=?, updated_at_ns=? WHERE name=?`, cursor.UTC().Format(time.RFC3339Nano), time.Now().UTC().UnixNano(), instance)
	if err != nil {
		return fmt.Errorf("commit reconciliation cursor: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count reconciliation cursor update: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("Arr instance %q not found", instance)
	}
	return nil
}

// CommitReconciliation applies a complete history page and advances its cursor
// in one transaction. A failed media mutation therefore cannot create a gap in
// the next history request.
func (r *Repository) CommitReconciliation(ctx context.Context, instance string, cursor time.Time, media []domain.Media, languages []domain.Language) error {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reconciliation commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range media {
		mediaID, changed, err := upsertMediaTx(ctx, tx, item, cursor)
		if err != nil {
			return err
		}
		if changed {
			if _, err := tx.ExecContext(ctx, `DELETE FROM candidates WHERE media_id=?`, mediaID); err != nil {
				return fmt.Errorf("invalidate reconciled candidates: %w", err)
			}
		}
		for _, language := range languages {
			if _, err := tx.ExecContext(ctx, `INSERT INTO search_states(media_id, language, state, attempt, failure_attempt, next_attempt_at_ns) VALUES (?, ?, 'pending', 0, 0, ?) ON CONFLICT(media_id, language) DO UPDATE SET state='pending', attempt=0, failure_attempt=0, next_attempt_at_ns=excluded.next_attempt_at_ns, last_outcome='', lease_owner=NULL, lease_until_ns=NULL`, mediaID, language.String(), cursor.UnixNano()); err != nil {
				return fmt.Errorf("reset reconciled media search: %w", err)
			}
		}
		eventID := fmt.Sprintf("reconcile:%s:%s:%d:%d", instance, item.Ref.Kind, item.Ref.FileID, item.Fingerprint.ModTime.UnixNano())
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(event_id, event_type, instance, kind, file_id, media_id, outcome, created_at_ns) VALUES (?, 'reconcile', ?, ?, ?, ?, 'applied', ?) ON CONFLICT DO NOTHING`, eventID, instance, item.Ref.Kind, item.Ref.FileID, mediaID, cursor.UnixNano()); err != nil {
			return fmt.Errorf("audit reconciled media: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE instances SET reconciliation_cursor=?, updated_at_ns=? WHERE name=?`, cursor.UTC().Format(time.RFC3339Nano), cursor.UnixNano(), instance)
	if err != nil {
		return fmt.Errorf("advance reconciliation cursor: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count reconciliation update: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("Arr instance %q not found", instance)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reconciliation page: %w", err)
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

func allSeriesKeys(ids domain.ExternalIDs, title string, year int) []string {
	keys := make([]string, 0, 4)
	if ids.TVDB != 0 {
		keys = append(keys, fmt.Sprintf("tvdb:%d", ids.TVDB))
	}
	if ids.TMDB != 0 {
		keys = append(keys, fmt.Sprintf("tmdb:%d", ids.TMDB))
	}
	if ids.IMDb != "" {
		keys = append(keys, "imdb:"+strings.ToLower(ids.IMDb))
	}
	keys = append(keys, fmt.Sprintf("title:%s:%d", strings.ToLower(strings.TrimSpace(title)), year))
	return keys
}

// StrongestSeriesKey exposes the canonical pack-cache lookup identity so the
// filesystem cache and repository cannot drift in how they address a series.
func StrongestSeriesKey(ids domain.ExternalIDs, title string, year int) string {
	return strongestSeriesKey(ids, title, year)
}
