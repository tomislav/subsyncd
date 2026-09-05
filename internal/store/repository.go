package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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

type SearchPriority int

const (
	SearchPriorityUpgrade SearchPriority = 100
	SearchPriorityMissing SearchPriority = 200
	SearchPriorityImport  SearchPriority = 300
)

func (p SearchPriority) String() string {
	switch p {
	case SearchPriorityUpgrade:
		return "upgrade"
	case SearchPriorityMissing:
		return "missing"
	case SearchPriorityImport:
		return "import"
	default:
		return "unknown"
	}
}

type SearchLease struct {
	MediaID        int64
	Language       string
	JobID          string
	Attempt        int
	FailureAttempt int
	LeaseUntil     time.Time
	Priority       SearchPriority
}

type SearchCompletion struct {
	JobID                 string
	Outcome               string
	NextAttemptAt         time.Time
	AdvanceMissingAttempt bool
	AdvanceFailureAttempt bool
	ResetMissingAttempt   bool
	ResetFailureAttempt   bool
	Priority              SearchPriority
}

type SearchCompletionResult struct {
	RerunScheduled bool
}

type NotificationRequest struct {
	Notifier      string
	DedupeKey     string
	PayloadJSON   []byte
	NextAttemptAt time.Time
}

type NotificationLease struct {
	ID          int64
	Notifier    string
	PayloadJSON []byte
	Attempt     int
	JobID       string
	LeaseUntil  time.Time
}

type NotificationCompletion struct {
	JobID         string
	Result        string
	NextAttemptAt time.Time
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

type CandidateRecord struct {
	ProviderID     string
	ResultID       string
	MetadataJSON   []byte
	ScoreJSON      []byte
	ValidationJSON []byte
}

type CandidateRejection struct {
	MediaID            int64
	Language           string
	ProviderID         string
	ResultID           string
	CandidateSignature string
	ArtifactChecksum   string
	ReasonCode         string
	ToolSignature      string
	MediaPath          string
	MediaFileID        int64
	MediaSize          int64
	MediaModTimeNS     int64
	RejectedAt         time.Time
	ExpiresAt          time.Time
}

type CandidateRejectionLookup struct {
	MediaID            int64
	Language           string
	ProviderID         string
	ResultID           string
	CandidateSignature string
	ArtifactChecksum   string
	ToolSignature      string
	MediaPath          string
	MediaFileID        int64
	MediaSize          int64
	MediaModTimeNS     int64
	Now                time.Time
}

type SearchStatus struct {
	State          string
	Attempt        int
	FailureAttempt int
	NextAttemptAt  time.Time
	LastOutcome    string
	Priority       SearchPriority
	RerunPending   bool
}

type MediaRecord struct {
	ID    int64
	Media domain.Media
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
	MediaPath      string
	MediaFileID    int64
	MediaSize      int64
	MediaModTimeNS int64
}

// MediaEventMutation is the persistence-level representation of an Arr event.
// Event IDs are globally unique so replayed webhooks remain no-ops.
type MediaEventMutation struct {
	EventID   string
	Type      string
	EntityID  int64
	Media     domain.Media
	Ref       domain.MediaRef
	Languages []domain.Language
	At        time.Time
	Priority  SearchPriority
}

func (r *Repository) UpsertMedia(ctx context.Context, media domain.Media) (int64, bool, error) {
	if media.EntityID <= 0 {
		return 0, false, fmt.Errorf("media entity ID must be positive")
	}
	if !validUnsupportedReason(media.UnsupportedReason) {
		return 0, false, fmt.Errorf("unsupported media reason %q", media.UnsupportedReason)
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin media upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	id, existingPath, existingFileID, existingSize, existingModTime, found, err := findMediaIdentityTx(ctx, tx, media)
	changed := true
	contentChanged := true
	if err != nil {
		return 0, false, err
	}
	if found {
		preserveAuthoritativeModTime(&media, existingFileID, existingSize, existingModTime)
		contentChanged = existingFileID != media.Fingerprint.FileID || existingSize != media.Fingerprint.Size || existingModTime != media.Fingerprint.ModTime.UnixNano()
		changed = existingPath != media.Fingerprint.Path || contentChanged
	}

	alternateTitles, err := json.Marshal(media.AlternateTitles)
	if err != nil {
		return 0, false, fmt.Errorf("encode alternate titles: %w", err)
	}
	now := time.Now().UTC().UnixNano()
	if !found {
		result, err := tx.ExecContext(ctx, `INSERT INTO media(instance, kind, file_id, entity_id, path, size, mod_time_ns, title, episode_title, alternate_titles_json, year, season, episode, absolute_episode, imdb_id, tmdb_id, tvdb_id, original_filename, release_name, release_group, source, resolution, streaming_service, edition, quality, duration_ns, unsupported_reason, updated_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			media.Ref.Instance, string(media.Ref.Kind), media.Ref.FileID, media.EntityID, media.Fingerprint.Path, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(), media.Title, media.EpisodeTitle, alternateTitles, media.Year, media.Season, media.Episode, media.AbsoluteEpisode, media.ExternalIDs.IMDb, media.ExternalIDs.TMDB, media.ExternalIDs.TVDB, media.OriginalFilename, media.ReleaseName, media.ReleaseGroup, media.Source, media.Resolution, media.StreamingService, media.Edition, media.Quality, int64(media.Duration), string(media.UnsupportedReason), now)
		if err != nil {
			return 0, false, fmt.Errorf("insert media: %w", err)
		}
		id, err = result.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("read media id: %w", err)
		}
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE media SET file_id=?, entity_id=?, path=?, size=?, mod_time_ns=?, title=?, episode_title=?, alternate_titles_json=?, year=?, season=?, episode=?, absolute_episode=?, imdb_id=?, tmdb_id=?, tvdb_id=?, original_filename=?, release_name=?, release_group=?, source=?, resolution=?, streaming_service=?, edition=?, quality=?, duration_ns=?, unsupported_reason=?, updated_at_ns=? WHERE id=?`,
			media.Ref.FileID, media.EntityID, media.Fingerprint.Path, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(), media.Title, media.EpisodeTitle, alternateTitles, media.Year, media.Season, media.Episode, media.AbsoluteEpisode, media.ExternalIDs.IMDb, media.ExternalIDs.TMDB, media.ExternalIDs.TVDB, media.OriginalFilename, media.ReleaseName, media.ReleaseGroup, media.Source, media.Resolution, media.StreamingService, media.Edition, media.Quality, int64(media.Duration), string(media.UnsupportedReason), now, id)
		if err != nil {
			return 0, false, fmt.Errorf("update media: %w", err)
		}
	}
	if contentChanged {
		if err := invalidateInstallationTx(ctx, tx, id); err != nil {
			return 0, false, err
		}
	} else if existingPath != media.Fingerprint.Path {
		if err := rebaseInstallationPathsTx(ctx, tx, id, existingPath, media.Fingerprint.Path); err != nil {
			return 0, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit media upsert: %w", err)
	}
	return id, changed, nil
}

func (r *Repository) GetMedia(ctx context.Context, mediaID int64) (domain.Media, error) {
	var media domain.Media
	var kind string
	var alternateTitles []byte
	var modTimeNS, durationNS int64
	var unsupportedReason string
	err := r.store.db.QueryRowContext(ctx, `SELECT instance, kind, file_id, entity_id, path, size, mod_time_ns, title, episode_title, alternate_titles_json, year, season, episode, absolute_episode, imdb_id, tmdb_id, tvdb_id, original_filename, release_name, release_group, source, resolution, streaming_service, edition, quality, duration_ns, unsupported_reason FROM media WHERE id=?`, mediaID).Scan(
		&media.Ref.Instance, &kind, &media.Ref.FileID, &media.EntityID, &media.Fingerprint.Path, &media.Fingerprint.Size, &modTimeNS, &media.Title, &media.EpisodeTitle, &alternateTitles, &media.Year, &media.Season, &media.Episode, &media.AbsoluteEpisode, &media.ExternalIDs.IMDb, &media.ExternalIDs.TMDB, &media.ExternalIDs.TVDB, &media.OriginalFilename, &media.ReleaseName, &media.ReleaseGroup, &media.Source, &media.Resolution, &media.StreamingService, &media.Edition, &media.Quality, &durationNS, &unsupportedReason)
	if err != nil {
		return domain.Media{}, fmt.Errorf("get media %d: %w", mediaID, err)
	}
	media.Ref.Kind = domain.MediaKind(kind)
	media.Fingerprint.FileID = media.Ref.FileID
	media.Fingerprint.ModTime = time.Unix(0, modTimeNS).UTC()
	media.Duration = time.Duration(durationNS)
	media.UnsupportedReason = domain.UnsupportedReason(unsupportedReason)
	if !validUnsupportedReason(media.UnsupportedReason) {
		return domain.Media{}, fmt.Errorf("get media %d: corrupt unsupported reason %q", mediaID, unsupportedReason)
	}
	if err := json.Unmarshal(alternateTitles, &media.AlternateTitles); err != nil {
		return domain.Media{}, fmt.Errorf("decode media %d alternate titles: %w", mediaID, err)
	}
	return media, nil
}

func (r *Repository) FindMedia(ctx context.Context, ref domain.MediaRef) (int64, domain.Media, error) {
	var mediaID int64
	err := r.store.db.QueryRowContext(ctx, `SELECT id FROM media WHERE instance=? AND kind=? AND file_id=?`, ref.Instance, string(ref.Kind), ref.FileID).Scan(&mediaID)
	if err != nil {
		return 0, domain.Media{}, fmt.Errorf("find media %s/%s/%d: %w", ref.Instance, ref.Kind, ref.FileID, err)
	}
	media, err := r.GetMedia(ctx, mediaID)
	return mediaID, media, err
}

func (r *Repository) FindMediaByEntity(ctx context.Context, instance string, kind domain.MediaKind, entityID int64) (int64, domain.Media, bool, error) {
	if entityID <= 0 {
		return 0, domain.Media{}, false, fmt.Errorf("media entity ID must be positive")
	}
	var mediaID int64
	err := r.store.db.QueryRowContext(ctx, `SELECT id FROM media WHERE instance=? AND kind=? AND entity_id=?`, instance, string(kind), entityID).Scan(&mediaID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, domain.Media{}, false, nil
	}
	if err != nil {
		return 0, domain.Media{}, false, fmt.Errorf("find media entity %s/%s/%d: %w", instance, kind, entityID, err)
	}
	media, err := r.GetMedia(ctx, mediaID)
	if err != nil {
		return 0, domain.Media{}, false, err
	}
	return mediaID, media, true, nil
}

func (r *Repository) ListMediaByInstance(ctx context.Context, instance string) ([]MediaRecord, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT id FROM media WHERE instance=? ORDER BY id`, instance)
	if err != nil {
		return nil, fmt.Errorf("list media for instance: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan media ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate media IDs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close media IDs: %w", err)
	}
	items := make([]MediaRecord, 0, len(ids))
	for _, id := range ids {
		media, err := r.GetMedia(ctx, id)
		if err != nil {
			return nil, err
		}
		items = append(items, MediaRecord{ID: id, Media: media})
	}
	return items, nil
}

func (r *Repository) GetSearchStatus(ctx context.Context, mediaID int64, language domain.Language) (SearchStatus, error) {
	var status SearchStatus
	var next int64
	err := r.store.db.QueryRowContext(ctx, `SELECT state, attempt, failure_attempt, next_attempt_at_ns, last_outcome, priority, rerun_requested FROM search_states WHERE media_id=? AND language=?`, mediaID, language.String()).Scan(&status.State, &status.Attempt, &status.FailureAttempt, &next, &status.LastOutcome, &status.Priority, &status.RerunPending)
	if err != nil {
		return SearchStatus{}, fmt.Errorf("get search status: %w", err)
	}
	if !validSearchPriority(status.Priority) {
		return SearchStatus{}, fmt.Errorf("get search status: corrupt priority %d", status.Priority)
	}
	status.NextAttemptAt = fromUnixNano(next)
	return status, nil
}

func (r *Repository) ListCandidates(ctx context.Context, mediaID int64, language domain.Language) ([]CandidateRecord, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT provider_id, result_id, metadata_json, score_json, validation_json FROM candidates WHERE media_id=? AND language=? ORDER BY id`, mediaID, language.String())
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
	}
	defer rows.Close()
	var candidates []CandidateRecord
	for rows.Next() {
		var candidate CandidateRecord
		if err := rows.Scan(&candidate.ProviderID, &candidate.ResultID, &candidate.MetadataJSON, &candidate.ScoreJSON, &candidate.ValidationJSON); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidates: %w", err)
	}
	return candidates, nil
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
	var existingFileID, existingSize, existingModTime int64
	err = tx.QueryRowContext(ctx, `SELECT file_id, size, mod_time_ns FROM media WHERE id=?`, mediaID).Scan(&existingFileID, &existingSize, &existingModTime)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("media %d not found while replacing track inventory", mediaID)
	}
	if err != nil {
		return fmt.Errorf("read inventory fingerprint: %w", err)
	}
	contentChanged := existingFileID != fingerprint.FileID || existingSize != fingerprint.Size || existingModTime != fingerprint.ModTime.UnixNano()
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
	if contentChanged {
		if err := invalidateInstallationTx(ctx, tx, mediaID); err != nil {
			return err
		}
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
	return r.UpsertSearchStateWithPriority(ctx, mediaID, language, next, SearchPriorityMissing)
}

func (r *Repository) UpsertSearchStateWithPriority(ctx context.Context, mediaID int64, language domain.Language, next time.Time, priority SearchPriority) error {
	if !validSearchPriority(priority) {
		return fmt.Errorf("invalid search priority %d", priority)
	}
	_, err := r.store.db.ExecContext(ctx, `INSERT INTO search_states(media_id, language, state, attempt, failure_attempt, next_attempt_at_ns, priority) VALUES (?, ?, 'pending', 0, 0, ?, ?) ON CONFLICT(media_id, language) DO UPDATE SET state='pending', attempt=0, failure_attempt=0, next_attempt_at_ns=excluded.next_attempt_at_ns, priority=excluded.priority, rerun_requested=0, lease_owner=NULL, lease_until_ns=NULL`, mediaID, language.String(), next.UnixNano(), priority)
	if err != nil {
		return fmt.Errorf("upsert search state: %w", err)
	}
	return nil
}

func validSearchPriority(priority SearchPriority) bool {
	return priority == SearchPriorityUpgrade || priority == SearchPriorityMissing || priority == SearchPriorityImport
}

func validUnsupportedReason(reason domain.UnsupportedReason) bool {
	return reason == "" || reason == domain.UnsupportedMultiEpisode
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
	rows, err := tx.QueryContext(ctx, `SELECT media_id, language, attempt, failure_attempt, priority FROM search_states WHERE state = 'pending' AND next_attempt_at_ns <= ? AND (lease_until_ns IS NULL OR lease_until_ns <= ?) ORDER BY priority DESC, next_attempt_at_ns, media_id, language LIMIT ?`, now.UnixNano(), now.UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("select due searches: %w", err)
	}
	type due struct {
		mediaID        int64
		language       string
		attempt        int
		failureAttempt int
		priority       SearchPriority
	}
	var dueRows []due
	for rows.Next() {
		var item due
		if err := rows.Scan(&item.mediaID, &item.language, &item.attempt, &item.failureAttempt, &item.priority); err != nil {
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
			leases = append(leases, SearchLease{MediaID: item.mediaID, Language: item.language, JobID: jobID, Attempt: item.attempt, FailureAttempt: item.failureAttempt, LeaseUntil: leaseUntil, Priority: item.priority})
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit search leases: %w", err)
	}
	return leases, nil
}

func (r *Repository) RenewSearchLease(ctx context.Context, jobID string, now time.Time, duration time.Duration) error {
	if jobID == "" || duration <= 0 {
		return fmt.Errorf("search lease owner and duration are required")
	}
	result, err := r.store.db.ExecContext(ctx, `UPDATE search_states SET lease_until_ns=? WHERE lease_owner=?`, now.Add(duration).UnixNano(), jobID)
	if err != nil {
		return fmt.Errorf("renew search lease: %w", err)
	}
	return requireOneRow(result, "search lease "+jobID+" not found")
}

func (r *Repository) CompleteSearch(ctx context.Context, completion SearchCompletion) (SearchCompletionResult, error) {
	state := "complete"
	next := int64(0)
	if !completion.NextAttemptAt.IsZero() {
		state = "pending"
		next = completion.NextAttemptAt.UnixNano()
	}
	advanceMissing := 0
	if completion.AdvanceMissingAttempt {
		advanceMissing = 1
	}
	advanceFailure := 0
	if completion.AdvanceFailureAttempt {
		advanceFailure = 1
	}
	if completion.Priority != 0 && !validSearchPriority(completion.Priority) {
		return SearchCompletionResult{}, fmt.Errorf("invalid search priority %d", completion.Priority)
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return SearchCompletionResult{}, fmt.Errorf("begin search completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var rerunScheduled bool
	if err := tx.QueryRowContext(ctx, `SELECT rerun_requested FROM search_states WHERE lease_owner=?`, completion.JobID).Scan(&rerunScheduled); errors.Is(err, sql.ErrNoRows) {
		return SearchCompletionResult{}, fmt.Errorf("search lease %s not found", completion.JobID)
	} else if err != nil {
		return SearchCompletionResult{}, fmt.Errorf("read search completion state: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE search_states SET
		state=CASE WHEN rerun_requested=1 THEN 'pending' ELSE ? END,
		attempt=CASE WHEN rerun_requested=1 THEN 0 WHEN ? THEN 0 ELSE attempt+? END,
		failure_attempt=CASE WHEN rerun_requested=1 THEN 0 WHEN ? THEN 0 ELSE failure_attempt+? END,
		next_attempt_at_ns=CASE WHEN rerun_requested=1 THEN next_attempt_at_ns ELSE ? END,
		last_outcome=CASE WHEN rerun_requested=1 THEN '' ELSE ? END,
		priority=CASE WHEN rerun_requested=1 OR ?=0 THEN priority ELSE ? END,
		rerun_requested=0, lease_owner=NULL, lease_until_ns=NULL
		WHERE lease_owner=?`, state, completion.ResetMissingAttempt, advanceMissing, completion.ResetFailureAttempt, advanceFailure, next, completion.Outcome, completion.Priority, completion.Priority, completion.JobID)
	if err != nil {
		return SearchCompletionResult{}, fmt.Errorf("complete search: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return SearchCompletionResult{}, fmt.Errorf("count completed search: %w", err)
	}
	if count != 1 {
		return SearchCompletionResult{}, fmt.Errorf("search lease %s not found", completion.JobID)
	}
	if err := tx.Commit(); err != nil {
		return SearchCompletionResult{}, fmt.Errorf("commit search completion: %w", err)
	}
	return SearchCompletionResult{RerunScheduled: rerunScheduled}, nil
}

func (r *Repository) EnqueueNotification(ctx context.Context, request NotificationRequest) (bool, error) {
	if request.Notifier == "" || request.DedupeKey == "" || !json.Valid(request.PayloadJSON) || request.NextAttemptAt.IsZero() {
		return false, fmt.Errorf("notification notifier, dedupe key, valid payload, and due time are required")
	}
	result, err := r.store.db.ExecContext(ctx, `INSERT INTO notifications(notifier, payload_json, attempt, next_attempt_at_ns, result, dedupe_key) VALUES (?, ?, 0, ?, '', ?) ON CONFLICT DO NOTHING`, request.Notifier, request.PayloadJSON, request.NextAttemptAt.UnixNano(), request.DedupeKey)
	if err != nil {
		return false, fmt.Errorf("enqueue notification: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count enqueued notification: %w", err)
	}
	return count == 1, nil
}

func (r *Repository) LeaseDueNotifications(ctx context.Context, now time.Time, limit int, duration time.Duration) ([]NotificationLease, error) {
	if limit <= 0 || duration <= 0 {
		return nil, fmt.Errorf("notification lease limit and duration must be positive")
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin notification lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id, notifier, payload_json, attempt FROM notifications WHERE next_attempt_at_ns > 0 AND next_attempt_at_ns <= ? AND (lease_until_ns IS NULL OR lease_until_ns <= ?) ORDER BY next_attempt_at_ns, id LIMIT ?`, now.UnixNano(), now.UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("select due notifications: %w", err)
	}
	var due []NotificationLease
	for rows.Next() {
		var item NotificationLease
		if err := rows.Scan(&item.ID, &item.Notifier, &item.PayloadJSON, &item.Attempt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan due notification: %w", err)
		}
		due = append(due, item)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close due notifications: %w", err)
	}
	leaseUntil := now.Add(duration)
	leased := make([]NotificationLease, 0, len(due))
	for _, item := range due {
		jobID, err := randomID()
		if err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE notifications SET lease_owner=?, lease_until_ns=? WHERE id=? AND (lease_until_ns IS NULL OR lease_until_ns <= ?)`, jobID, leaseUntil.UnixNano(), item.ID, now.UnixNano())
		if err != nil {
			return nil, fmt.Errorf("claim due notification: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("count claimed notification: %w", err)
		}
		if count == 1 {
			item.JobID = jobID
			item.LeaseUntil = leaseUntil
			leased = append(leased, item)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit notification leases: %w", err)
	}
	return leased, nil
}

func (r *Repository) RenewNotificationLease(ctx context.Context, jobID string, now time.Time, duration time.Duration) error {
	if jobID == "" || duration <= 0 {
		return fmt.Errorf("notification lease owner and duration are required")
	}
	result, err := r.store.db.ExecContext(ctx, `UPDATE notifications SET lease_until_ns=? WHERE lease_owner=?`, now.Add(duration).UnixNano(), jobID)
	if err != nil {
		return fmt.Errorf("renew notification lease: %w", err)
	}
	return requireOneRow(result, "notification lease "+jobID+" not found")
}

func (r *Repository) CompleteNotification(ctx context.Context, completion NotificationCompletion) error {
	next := int64(0)
	if !completion.NextAttemptAt.IsZero() {
		next = completion.NextAttemptAt.UnixNano()
	}
	result, err := r.store.db.ExecContext(ctx, `UPDATE notifications SET attempt=attempt+1, next_attempt_at_ns=?, result=?, lease_owner=NULL, lease_until_ns=NULL WHERE lease_owner=?`, next, completion.Result, completion.JobID)
	if err != nil {
		return fmt.Errorf("complete notification: %w", err)
	}
	return requireOneRow(result, "notification lease "+completion.JobID+" not found")
}

func requireOneRow(result sql.Result, message string) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New(message)
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

func (r *Repository) ClearProviderState(ctx context.Context, providerID string) error {
	if strings.TrimSpace(providerID) == "" {
		return fmt.Errorf("provider ID is required")
	}
	if _, err := r.store.db.ExecContext(ctx, `DELETE FROM provider_states WHERE provider_id=?`, providerID); err != nil {
		return fmt.Errorf("clear provider state: %w", err)
	}
	return nil
}

func (r *Repository) ListProviderStates(ctx context.Context, providerID string) ([]ProviderState, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT provider_id, scope, reason, quota_limit, quota_remaining, reset_at_ns, disabled, failure_attempt FROM provider_states WHERE provider_id=? ORDER BY scope`, providerID)
	if err != nil {
		return nil, fmt.Errorf("list provider states: %w", err)
	}
	defer rows.Close()
	var states []ProviderState
	for rows.Next() {
		var state ProviderState
		var reset int64
		if err := rows.Scan(&state.ProviderID, &state.Scope, &state.Reason, &state.Limit, &state.Remaining, &reset, &state.Disabled, &state.FailureAttempt); err != nil {
			return nil, fmt.Errorf("scan provider state: %w", err)
		}
		state.ResetAt = fromUnixNano(reset)
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider states: %w", err)
	}
	return states, nil
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

func (r *Repository) RecordCandidates(ctx context.Context, mediaID int64, language domain.Language, candidates []CandidateRecord) error {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin candidate replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM candidates WHERE media_id=? AND language=?`, mediaID, language.String()); err != nil {
		return fmt.Errorf("delete previous candidates: %w", err)
	}
	now := time.Now().UTC().UnixNano()
	for _, candidate := range candidates {
		if len(candidate.MetadataJSON) == 0 || len(candidate.ScoreJSON) == 0 {
			return fmt.Errorf("candidate metadata and score are required")
		}
		if len(candidate.ValidationJSON) == 0 {
			candidate.ValidationJSON = []byte(`{}`)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO candidates(media_id, language, provider_id, result_id, metadata_json, score_json, validation_json, created_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, mediaID, language.String(), candidate.ProviderID, candidate.ResultID, candidate.MetadataJSON, candidate.ScoreJSON, candidate.ValidationJSON, now); err != nil {
			return fmt.Errorf("insert candidate: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit candidate replacement: %w", err)
	}
	return nil
}

func (r *Repository) PutCandidateRejection(ctx context.Context, rejection CandidateRejection) error {
	if rejection.MediaID <= 0 || rejection.Language == "" || rejection.ProviderID == "" || rejection.ResultID == "" || rejection.CandidateSignature == "" || rejection.ReasonCode == "" || rejection.ToolSignature == "" || rejection.MediaPath == "" || rejection.RejectedAt.IsZero() || !rejection.ExpiresAt.After(rejection.RejectedAt) {
		return fmt.Errorf("candidate rejection identity, reason, fingerprint, and expiry are required")
	}
	_, err := r.store.db.ExecContext(ctx, `INSERT INTO candidate_rejections(media_id, language, provider_id, result_id, candidate_signature, artifact_checksum, reason_code, tool_signature, media_path, media_file_id, media_size, media_mod_time_ns, rejected_at_ns, expires_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(media_id, language, provider_id, result_id, candidate_signature, artifact_checksum, tool_signature) DO UPDATE SET reason_code=excluded.reason_code, media_path=excluded.media_path, media_file_id=excluded.media_file_id, media_size=excluded.media_size, media_mod_time_ns=excluded.media_mod_time_ns, rejected_at_ns=excluded.rejected_at_ns, expires_at_ns=excluded.expires_at_ns`, rejection.MediaID, rejection.Language, rejection.ProviderID, rejection.ResultID, rejection.CandidateSignature, rejection.ArtifactChecksum, rejection.ReasonCode, rejection.ToolSignature, rejection.MediaPath, rejection.MediaFileID, rejection.MediaSize, rejection.MediaModTimeNS, rejection.RejectedAt.UnixNano(), rejection.ExpiresAt.UnixNano())
	if err != nil {
		return fmt.Errorf("put candidate rejection: %w", err)
	}
	return nil
}

func (r *Repository) GetCandidateRejection(ctx context.Context, lookup CandidateRejectionLookup) (CandidateRejection, bool, error) {
	query := `SELECT media_id, language, provider_id, result_id, candidate_signature, artifact_checksum, reason_code, tool_signature, media_path, media_file_id, media_size, media_mod_time_ns, rejected_at_ns, expires_at_ns FROM candidate_rejections WHERE media_id=? AND language=? AND provider_id=? AND result_id=? AND candidate_signature=? AND tool_signature=? AND media_path=? AND media_file_id=? AND media_size=? AND media_mod_time_ns=? AND expires_at_ns>?`
	args := []any{lookup.MediaID, lookup.Language, lookup.ProviderID, lookup.ResultID, lookup.CandidateSignature, lookup.ToolSignature, lookup.MediaPath, lookup.MediaFileID, lookup.MediaSize, lookup.MediaModTimeNS, lookup.Now.UnixNano()}
	if lookup.ArtifactChecksum != "" {
		query += ` AND artifact_checksum=?`
		args = append(args, lookup.ArtifactChecksum)
	}
	query += ` ORDER BY rejected_at_ns DESC LIMIT 1`
	var rejection CandidateRejection
	var rejectedAt, expiresAt int64
	err := r.store.db.QueryRowContext(ctx, query, args...).Scan(&rejection.MediaID, &rejection.Language, &rejection.ProviderID, &rejection.ResultID, &rejection.CandidateSignature, &rejection.ArtifactChecksum, &rejection.ReasonCode, &rejection.ToolSignature, &rejection.MediaPath, &rejection.MediaFileID, &rejection.MediaSize, &rejection.MediaModTimeNS, &rejectedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CandidateRejection{}, false, nil
	}
	if err != nil {
		return CandidateRejection{}, false, fmt.Errorf("get candidate rejection: %w", err)
	}
	rejection.RejectedAt = fromUnixNano(rejectedAt)
	rejection.ExpiresAt = fromUnixNano(expiresAt)
	return rejection, true, nil
}

func (r *Repository) ListCandidateRejections(ctx context.Context, mediaID int64, language domain.Language, now time.Time) ([]CandidateRejection, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT media_id, language, provider_id, result_id, candidate_signature, artifact_checksum, reason_code, tool_signature, media_path, media_file_id, media_size, media_mod_time_ns, rejected_at_ns, expires_at_ns FROM candidate_rejections WHERE media_id=? AND language=? AND expires_at_ns>? ORDER BY rejected_at_ns DESC`, mediaID, language.String(), now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("list candidate rejections: %w", err)
	}
	defer rows.Close()
	var rejections []CandidateRejection
	for rows.Next() {
		var rejection CandidateRejection
		var rejectedAt, expiresAt int64
		if err := rows.Scan(&rejection.MediaID, &rejection.Language, &rejection.ProviderID, &rejection.ResultID, &rejection.CandidateSignature, &rejection.ArtifactChecksum, &rejection.ReasonCode, &rejection.ToolSignature, &rejection.MediaPath, &rejection.MediaFileID, &rejection.MediaSize, &rejection.MediaModTimeNS, &rejectedAt, &expiresAt); err != nil {
			return nil, fmt.Errorf("scan candidate rejection: %w", err)
		}
		rejection.RejectedAt = fromUnixNano(rejectedAt)
		rejection.ExpiresAt = fromUnixNano(expiresAt)
		rejections = append(rejections, rejection)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidate rejections: %w", err)
	}
	return rejections, nil
}

func (r *Repository) ClearCandidateRejections(ctx context.Context, mediaID int64, language domain.Language) error {
	if _, err := r.store.db.ExecContext(ctx, `DELETE FROM candidate_rejections WHERE media_id=? AND language=?`, mediaID, language.String()); err != nil {
		return fmt.Errorf("clear candidate rejections: %w", err)
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

func (r *Repository) ListReusablePacks(ctx context.Context, lookup PackLookup, now time.Time) ([]PackCacheEntry, error) {
	seriesKeys := allSeriesKeys(lookup.SeriesIDs, lookup.SeriesTitle, lookup.SeriesYear)
	args := make([]any, 0, len(seriesKeys)+4)
	for _, key := range seriesKeys {
		args = append(args, key)
	}
	args = append(args, lookup.Season, lookup.Language, now.UnixNano())
	providerClause := ""
	if lookup.ProviderID != "" {
		providerClause = " AND provider_id=?"
		args = append(args, lookup.ProviderID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(seriesKeys)), ",")
	query := `SELECT id, provider_id, result_id, series_key, season, language, content_checksum, manifest_path, candidate_json, byte_size, expires_at_ns, last_access_at_ns FROM pack_cache WHERE series_key IN (` + placeholders + `) AND season=? AND language=? AND expires_at_ns>?` + providerClause + ` ORDER BY last_access_at_ns DESC, id`
	rows, err := r.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list reusable packs: %w", err)
	}
	defer rows.Close()
	var entries []PackCacheEntry
	for rows.Next() {
		var entry PackCacheEntry
		var expires, lastAccess int64
		if err := rows.Scan(&entry.ID, &entry.ProviderID, &entry.ResultID, &entry.SeriesKey, &entry.Season, &entry.Language, &entry.ContentChecksum, &entry.ManifestPath, &entry.CandidateJSON, &entry.ByteSize, &expires, &lastAccess); err != nil {
			return nil, fmt.Errorf("scan reusable pack: %w", err)
		}
		entry.ExpiresAt = fromUnixNano(expires)
		entry.LastAccessAt = fromUnixNano(lastAccess)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reusable packs: %w", err)
	}
	return entries, nil
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
	err := r.store.db.QueryRowContext(ctx, `SELECT media_id, language, path, checksum, provider_id, candidate_id, score_json, sync_result_json, rollback_path, media_path, media_file_id, media_size, media_mod_time_ns FROM installations WHERE media_id=? AND language=?`, mediaID, language.String()).Scan(&installation.MediaID, &installation.Language, &installation.Path, &installation.Checksum, &installation.ProviderID, &installation.CandidateID, &installation.ScoreJSON, &installation.SyncResultJSON, &installation.RollbackPath, &installation.MediaPath, &installation.MediaFileID, &installation.MediaSize, &installation.MediaModTimeNS)
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
	var unsupportedReason string
	if err := tx.QueryRowContext(ctx, `SELECT unsupported_reason FROM media WHERE id=?`, installation.MediaID).Scan(&unsupportedReason); err != nil {
		return fmt.Errorf("read installation media support: %w", err)
	}
	if reason := domain.UnsupportedReason(unsupportedReason); !validUnsupportedReason(reason) {
		return fmt.Errorf("read installation media support: corrupt unsupported reason %q", unsupportedReason)
	} else if reason != "" {
		return fmt.Errorf("record installation: media is unsupported: %s", reason)
	}
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
	_, err = tx.ExecContext(ctx, `INSERT INTO installations(media_id, language, path, checksum, provider_id, candidate_id, score_json, sync_result_json, rollback_path, media_path, media_file_id, media_size, media_mod_time_ns, installed_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(media_id, language) DO UPDATE SET path=excluded.path, checksum=excluded.checksum, provider_id=excluded.provider_id, candidate_id=excluded.candidate_id, score_json=excluded.score_json, sync_result_json=excluded.sync_result_json, rollback_path=excluded.rollback_path, media_path=excluded.media_path, media_file_id=excluded.media_file_id, media_size=excluded.media_size, media_mod_time_ns=excluded.media_mod_time_ns, installed_at_ns=excluded.installed_at_ns`, installation.MediaID, installation.Language, installation.Path, installation.Checksum, installation.ProviderID, installation.CandidateID, installation.ScoreJSON, installation.SyncResultJSON, installation.RollbackPath, installation.MediaPath, installation.MediaFileID, installation.MediaSize, installation.MediaModTimeNS, now)
	if err != nil {
		return fmt.Errorf("record installation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit installation: %w", err)
	}
	return nil
}

// UpdateInstallationAssessment refreshes scoring or exact-hash provenance
// without rewriting the installed subtitle or creating a new install event.
// The full artifact and media identity guard prevents stale workers from
// updating a replacement installation.
func (r *Repository) UpdateInstallationAssessment(ctx context.Context, installation Installation) error {
	result, err := r.store.db.ExecContext(ctx, `UPDATE installations SET score_json=?, sync_result_json=? WHERE media_id=? AND language=? AND provider_id=? AND candidate_id=? AND checksum=? AND media_path=? AND media_file_id=? AND media_size=? AND media_mod_time_ns=?`,
		installation.ScoreJSON, installation.SyncResultJSON, installation.MediaID, installation.Language, installation.ProviderID, installation.CandidateID, installation.Checksum, installation.MediaPath, installation.MediaFileID, installation.MediaSize, installation.MediaModTimeNS)
	if err != nil {
		return fmt.Errorf("update installation assessment: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count installation assessment update: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("installation changed before assessment update")
	}
	return nil
}

func (r *Repository) ApplyMediaEvent(ctx context.Context, mutation MediaEventMutation) (bool, error) {
	if mutation.At.IsZero() {
		mutation.At = time.Now().UTC()
	}
	if mutation.Priority == 0 {
		mutation.Priority = SearchPriorityImport
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin media event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	applied, err := applyMediaMutationTx(ctx, tx, mutation)
	if err != nil {
		return false, err
	}
	if !applied {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id IN (SELECT id FROM events ORDER BY created_at_ns DESC, id DESC LIMIT -1 OFFSET 10000)`); err != nil {
		return false, fmt.Errorf("bound media audit log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit media event: %w", err)
	}
	return true, nil
}

func applyMediaMutationTx(ctx context.Context, tx *sql.Tx, mutation MediaEventMutation) (bool, error) {
	if err := validateMediaMutation(mutation); err != nil {
		return false, err
	}
	if !validSearchPriority(mutation.Priority) {
		return false, fmt.Errorf("invalid search priority %d", mutation.Priority)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO events(event_id, event_type, instance, kind, file_id, entity_id, outcome, created_at_ns) VALUES (?, ?, ?, ?, ?, ?, 'applied', ?) ON CONFLICT DO NOTHING`, mutation.EventID, mutation.Type, mutation.Ref.Instance, mutation.Ref.Kind, mutation.Ref.FileID, mutation.EntityID, mutation.At.UnixNano())
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
		if mutation.Media.Ref != mutation.Ref {
			return false, fmt.Errorf("media event reference does not match hydrated media")
		}
		mediaID, contentChanged, err := upsertMediaTx(ctx, tx, mutation.Media, mutation.At)
		if err != nil {
			return false, err
		}
		if contentChanged {
			if _, err := tx.ExecContext(ctx, `DELETE FROM candidates WHERE media_id=?`, mediaID); err != nil {
				return false, fmt.Errorf("invalidate media candidates: %w", err)
			}
			if err := invalidateInstallationTx(ctx, tx, mediaID); err != nil {
				return false, err
			}
		}
		for _, language := range mutation.Languages {
			if err := scheduleMediaSearchTx(ctx, tx, mediaID, language, mutation.At, mutation.Priority, mutation.Media.UnsupportedReason); err != nil {
				return false, fmt.Errorf("reset media search: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE events SET media_id=? WHERE event_id=?`, mediaID, mutation.EventID); err != nil {
			return false, fmt.Errorf("link media event: %w", err)
		}
	case "delete":
		var mediaID, fileID, entityID int64
		var err error
		if mutation.Ref.FileID > 0 {
			err = tx.QueryRowContext(ctx, `SELECT id, file_id, entity_id FROM media WHERE instance=? AND kind=? AND file_id=?`, mutation.Ref.Instance, mutation.Ref.Kind, mutation.Ref.FileID).Scan(&mediaID, &fileID, &entityID)
		} else {
			err = tx.QueryRowContext(ctx, `SELECT id, file_id, entity_id FROM media WHERE instance=? AND kind=? AND entity_id=?`, mutation.Ref.Instance, mutation.Ref.Kind, mutation.EntityID).Scan(&mediaID, &fileID, &entityID)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("find deleted media: %w", err)
		}
		if err == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE search_states SET state='complete', last_outcome='deleted', rerun_requested=0, lease_owner=NULL, lease_until_ns=NULL WHERE media_id=?`, mediaID); err != nil {
				return false, fmt.Errorf("cancel deleted media searches: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE events SET media_id=?, file_id=?, entity_id=? WHERE event_id=?`, mediaID, fileID, entityID, mutation.EventID); err != nil {
				return false, fmt.Errorf("link delete event: %w", err)
			}
		}
	default:
		return false, fmt.Errorf("unsupported media event type %q", mutation.Type)
	}
	return true, nil
}

func validateMediaMutation(mutation MediaEventMutation) error {
	if mutation.EventID == "" || mutation.Ref.Instance == "" || (mutation.Ref.Kind != domain.MediaMovie && mutation.Ref.Kind != domain.MediaEpisode) || mutation.At.IsZero() {
		return fmt.Errorf("media event identity is incomplete")
	}
	switch mutation.Type {
	case "import", "rename":
		if mutation.EntityID <= 0 || mutation.Ref.FileID <= 0 || mutation.Media.EntityID != mutation.EntityID || mutation.Media.Ref != mutation.Ref {
			return fmt.Errorf("media event identity is incomplete")
		}
	case "delete":
		if mutation.Ref.FileID <= 0 && mutation.EntityID <= 0 {
			return fmt.Errorf("media event identity is incomplete")
		}
	default:
		return fmt.Errorf("unsupported media event type %q", mutation.Type)
	}
	return nil
}

func scheduleMediaSearchTx(ctx context.Context, tx *sql.Tx, mediaID int64, language domain.Language, at time.Time, priority SearchPriority, unsupported domain.UnsupportedReason) error {
	if !validSearchPriority(priority) {
		return fmt.Errorf("invalid search priority %d", priority)
	}
	if !validUnsupportedReason(unsupported) {
		return fmt.Errorf("invalid unsupported media reason %q", unsupported)
	}
	if unsupported == "" {
		_, err := tx.ExecContext(ctx, `INSERT INTO search_states(media_id, language, state, attempt, failure_attempt, next_attempt_at_ns, priority) VALUES (?, ?, 'pending', 0, 0, ?, ?) ON CONFLICT(media_id, language) DO UPDATE SET state='pending', attempt=0, failure_attempt=0, next_attempt_at_ns=excluded.next_attempt_at_ns, last_outcome='', priority=MAX(search_states.priority, excluded.priority), rerun_requested=CASE WHEN search_states.lease_owner IS NULL THEN 0 ELSE 1 END, lease_owner=search_states.lease_owner, lease_until_ns=search_states.lease_until_ns`, mediaID, language.String(), at.UnixNano(), priority)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO search_states(media_id, language, state, attempt, failure_attempt, next_attempt_at_ns, last_outcome, priority) VALUES (?, ?, 'complete', 0, 0, 0, ?, ?) ON CONFLICT(media_id, language) DO UPDATE SET state=CASE WHEN search_states.lease_owner IS NULL THEN 'complete' ELSE 'pending' END, attempt=0, failure_attempt=0, next_attempt_at_ns=CASE WHEN search_states.lease_owner IS NULL THEN 0 ELSE ? END, last_outcome=CASE WHEN search_states.lease_owner IS NULL THEN ? ELSE '' END, priority=MAX(search_states.priority, excluded.priority), rerun_requested=CASE WHEN search_states.lease_owner IS NULL THEN 0 ELSE 1 END, lease_owner=search_states.lease_owner, lease_until_ns=search_states.lease_until_ns`, mediaID, language.String(), string(unsupported), priority, at.UnixNano(), string(unsupported))
	return err
}

func upsertMediaTx(ctx context.Context, tx *sql.Tx, media domain.Media, at time.Time) (int64, bool, error) {
	if media.EntityID <= 0 {
		return 0, false, fmt.Errorf("media entity ID must be positive")
	}
	if !validUnsupportedReason(media.UnsupportedReason) {
		return 0, false, fmt.Errorf("unsupported media reason %q", media.UnsupportedReason)
	}
	id, existingPath, existingFileID, existingSize, existingModTime, found, err := findMediaIdentityTx(ctx, tx, media)
	changed := true
	if err != nil {
		return 0, false, err
	}
	if found {
		preserveAuthoritativeModTime(&media, existingFileID, existingSize, existingModTime)
		changed = existingFileID != media.Fingerprint.FileID || existingSize != media.Fingerprint.Size || existingModTime != media.Fingerprint.ModTime.UnixNano()
	}
	alternateTitles, err := json.Marshal(media.AlternateTitles)
	if err != nil {
		return 0, false, fmt.Errorf("encode media alternate titles: %w", err)
	}
	if !found {
		result, err := tx.ExecContext(ctx, `INSERT INTO media(instance, kind, file_id, entity_id, path, size, mod_time_ns, title, episode_title, alternate_titles_json, year, season, episode, absolute_episode, imdb_id, tmdb_id, tvdb_id, original_filename, release_name, release_group, source, resolution, streaming_service, edition, quality, duration_ns, unsupported_reason, updated_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, media.Ref.Instance, media.Ref.Kind, media.Ref.FileID, media.EntityID, media.Fingerprint.Path, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(), media.Title, media.EpisodeTitle, alternateTitles, media.Year, media.Season, media.Episode, media.AbsoluteEpisode, media.ExternalIDs.IMDb, media.ExternalIDs.TMDB, media.ExternalIDs.TVDB, media.OriginalFilename, media.ReleaseName, media.ReleaseGroup, media.Source, media.Resolution, media.StreamingService, media.Edition, media.Quality, int64(media.Duration), string(media.UnsupportedReason), at.UnixNano())
		if err != nil {
			return 0, false, fmt.Errorf("insert media for event: %w", err)
		}
		id, err = result.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("read media ID for event: %w", err)
		}
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE media SET file_id=?, entity_id=?, path=?, size=?, mod_time_ns=?, title=?, episode_title=?, alternate_titles_json=?, year=?, season=?, episode=?, absolute_episode=?, imdb_id=?, tmdb_id=?, tvdb_id=?, original_filename=?, release_name=?, release_group=?, source=?, resolution=?, streaming_service=?, edition=?, quality=?, duration_ns=?, unsupported_reason=?, updated_at_ns=? WHERE id=?`, media.Ref.FileID, media.EntityID, media.Fingerprint.Path, media.Fingerprint.Size, media.Fingerprint.ModTime.UnixNano(), media.Title, media.EpisodeTitle, alternateTitles, media.Year, media.Season, media.Episode, media.AbsoluteEpisode, media.ExternalIDs.IMDb, media.ExternalIDs.TMDB, media.ExternalIDs.TVDB, media.OriginalFilename, media.ReleaseName, media.ReleaseGroup, media.Source, media.Resolution, media.StreamingService, media.Edition, media.Quality, int64(media.Duration), string(media.UnsupportedReason), at.UnixNano(), id)
		if err != nil {
			return 0, false, fmt.Errorf("update media for event: %w", err)
		}
		if !changed && existingPath != media.Fingerprint.Path {
			if err := rebaseInstallationPathsTx(ctx, tx, id, existingPath, media.Fingerprint.Path); err != nil {
				return 0, false, err
			}
		}
	}
	return id, changed, nil
}

func findMediaIdentityTx(ctx context.Context, tx *sql.Tx, media domain.Media) (id int64, existingPath string, existingFileID, existingSize, existingModTime int64, found bool, err error) {
	type identityRow struct {
		id       int64
		path     string
		fileID   int64
		entityID int64
		size     int64
		modTime  int64
	}
	read := func(query string, args ...any) (identityRow, bool, error) {
		var row identityRow
		err := tx.QueryRowContext(ctx, query, args...).Scan(&row.id, &row.path, &row.fileID, &row.entityID, &row.size, &row.modTime)
		if errors.Is(err, sql.ErrNoRows) {
			return identityRow{}, false, nil
		}
		if err != nil {
			return identityRow{}, false, err
		}
		return row, true, nil
	}

	entityRow, entityFound, err := read(
		`SELECT id, path, file_id, entity_id, size, mod_time_ns FROM media WHERE instance=? AND kind=? AND entity_id=?`,
		media.Ref.Instance, media.Ref.Kind, media.EntityID,
	)
	if err != nil {
		return 0, "", 0, 0, 0, false, fmt.Errorf("find media by entity: %w", err)
	}
	fileRow, fileFound, err := read(
		`SELECT id, path, file_id, entity_id, size, mod_time_ns FROM media WHERE instance=? AND kind=? AND file_id=?`,
		media.Ref.Instance, media.Ref.Kind, media.Ref.FileID,
	)
	if err != nil {
		return 0, "", 0, 0, 0, false, fmt.Errorf("find media by file: %w", err)
	}
	if entityFound && fileFound && entityRow.id != fileRow.id {
		return 0, "", 0, 0, 0, false, fmt.Errorf("conflicting media identities")
	}
	if entityFound {
		return entityRow.id, entityRow.path, entityRow.fileID, entityRow.size, entityRow.modTime, true, nil
	}
	if fileFound {
		return 0, "", 0, 0, 0, false, fmt.Errorf("conflicting media identities")
	}
	return 0, "", 0, 0, 0, false, nil
}

func preserveAuthoritativeModTime(media *domain.Media, existingFileID, existingSize, existingModTime int64) {
	if existingFileID == media.Fingerprint.FileID && existingSize == media.Fingerprint.Size {
		media.Fingerprint.ModTime = time.Unix(0, existingModTime).UTC()
	}
}

func invalidateInstallationTx(ctx context.Context, tx *sql.Tx, mediaID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE installations SET score_json='{}', sync_result_json='{}', media_path='', media_file_id=0, media_size=0, media_mod_time_ns=0 WHERE media_id=?`, mediaID)
	if err != nil {
		return fmt.Errorf("invalidate installation provenance: %w", err)
	}
	return nil
}

func rebaseInstallationPathsTx(ctx context.Context, tx *sql.Tx, mediaID int64, oldMediaPath, newMediaPath string) error {
	type installationPaths struct {
		language string
		path     string
		rollback string
	}
	rows, err := tx.QueryContext(ctx, `SELECT language, path, rollback_path FROM installations WHERE media_id=?`, mediaID)
	if err != nil {
		return fmt.Errorf("list installation paths for media rename: %w", err)
	}
	var installations []installationPaths
	for rows.Next() {
		var paths installationPaths
		if err := rows.Scan(&paths.language, &paths.path, &paths.rollback); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan installation paths for media rename: %w", err)
		}
		installations = append(installations, paths)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate installation paths for media rename: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close installation paths for media rename: %w", err)
	}
	for _, installation := range installations {
		path := rebaseSidecarPath(installation.path, oldMediaPath, newMediaPath)
		rollback := rebaseSiblingPath(installation.rollback, oldMediaPath, newMediaPath)
		if _, err := tx.ExecContext(ctx, `UPDATE installations SET path=?, rollback_path=?, media_path=? WHERE media_id=? AND language=?`, path, rollback, newMediaPath, mediaID, installation.language); err != nil {
			return fmt.Errorf("rebase installation paths for media rename: %w", err)
		}
	}
	return nil
}

func rebaseSidecarPath(sidecar, oldMediaPath, newMediaPath string) string {
	if filepath.Clean(filepath.Dir(sidecar)) != filepath.Clean(filepath.Dir(oldMediaPath)) {
		return sidecar
	}
	oldStem := strings.TrimSuffix(filepath.Base(oldMediaPath), filepath.Ext(oldMediaPath))
	base := filepath.Base(sidecar)
	prefix := oldStem + "."
	if !strings.HasPrefix(base, prefix) {
		return sidecar
	}
	newStem := strings.TrimSuffix(filepath.Base(newMediaPath), filepath.Ext(newMediaPath))
	return filepath.Join(filepath.Dir(newMediaPath), newStem+strings.TrimPrefix(base, oldStem))
}

func rebaseSiblingPath(path, oldMediaPath, newMediaPath string) string {
	if path == "" || filepath.Clean(filepath.Dir(path)) != filepath.Clean(filepath.Dir(oldMediaPath)) {
		return path
	}
	return filepath.Join(filepath.Dir(newMediaPath), filepath.Base(path))
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
func (r *Repository) CommitReconciliation(ctx context.Context, instance string, cursor time.Time, mutations []MediaEventMutation) error {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reconciliation commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, mutation := range mutations {
		if mutation.Ref.Instance != instance {
			return fmt.Errorf("reconciliation mutation instance %q does not match %q", mutation.Ref.Instance, instance)
		}
		if mutation.Priority == 0 {
			mutation.Priority = SearchPriorityMissing
		}
		if _, err := applyMediaMutationTx(ctx, tx, mutation); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id IN (SELECT id FROM events ORDER BY created_at_ns DESC, id DESC LIMIT -1 OFFSET 10000)`); err != nil {
		return fmt.Errorf("bound media audit log: %w", err)
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
