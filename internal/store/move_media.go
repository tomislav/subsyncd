package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"subsyncd/internal/domain"
)

// ErrMediaMoveRefused reports that at least one entry of a media move has a
// problem, so nothing was moved. The returned items say which and why.
var ErrMediaMoveRefused = errors.New("media move refused")

// MediaMoveEntry is one entry of a move plan: which file of the source
// instance it is, and its identity and local path under the target instance.
// A movie is named by TMDBID, an episode by TVDBID (the series), Season and
// Episode; FromFileID names the source file directly and, given beside an
// identity, must agree with it.
type MediaMoveEntry struct {
	Kind       domain.MediaKind
	TMDBID     int64
	TVDBID     int64
	Season     int
	Episode    int
	FromFileID int64
	ToEntityID int64
	ToFileID   int64
	ToSeriesID int64
	// ToPath is the media file's path in this container, already mapped.
	ToPath string
}

// MovedSidecar is an installation's sidecar before and after a move.
type MovedSidecar struct {
	Language     string
	From, To     string
	Checksum     string
	RollbackFrom string
	RollbackTo   string
}

// MediaMoveItem describes one plan entry: the source row it matched, the
// paths it moves, and the problems that stop the move.
type MediaMoveItem struct {
	// Entry is the 1-based position in the plan; Plan is the entry itself.
	Entry        int
	Plan         MediaMoveEntry
	MediaID      int64
	Title        string
	Year         int
	Season       int
	Episode      int
	FromFileID   int64
	FromEntityID int64
	FromPath     string
	ToPath       string
	Size         int64
	Sidecars     []MovedSidecar
	Tracks       int
	// PendingNotifications counts outbox notifications not yet delivered
	// that still name the source file. They are left as they are.
	PendingNotifications int
	Problems             []string
}

// MediaMoveCheck inspects the files of an item (the new media path, each
// sidecar at its new path) and appends any problem to item.Problems. It runs
// inside the move's transaction, before anything is written.
type MediaMoveCheck func(context.Context, *MediaMoveItem)

// MoveMedia moves media rows from the instance from to the instance to in
// place, in one transaction: each row keeps its id, so every child row
// (installations, tracks, searches, candidates, rejections, hashes, history)
// stays attached. Each entry's row takes the target's instance, entity ID,
// file ID, series ID and path; its installations take the new media path and
// file ID and their sidecar (and rollback copy) paths in the new media
// folder; its external tracks, hashes, rejections and LAPSE failures recorded
// for the source file follow it. The inventory probe is invalidated by the
// media trigger, so the next refresh probes the file once.
//
// Every entry is validated first (the source row, conflicts under to, the
// recorded installation provenance, then check for the files). Any problem
// refuses the whole move with ErrMediaMoveRefused and the items. With apply
// false nothing is written and the items are a preview.
func (r *Repository) MoveMedia(ctx context.Context, from, to string, entries []MediaMoveEntry, check MediaMoveCheck, apply bool) ([]MediaMoveItem, error) {
	if from == "" || to == "" || from == to {
		return nil, fmt.Errorf("media move needs two different instances")
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin media move: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var foreignKeys int
	if err := tx.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return nil, fmt.Errorf("check foreign keys: %w", err)
	}
	if foreignKeys != 1 {
		return nil, errors.New("media move requires SQLite foreign key enforcement")
	}

	items := make([]MediaMoveItem, len(entries))
	sources := map[int64]int{}
	toFiles := map[string]int{}
	toEntities := map[string]int{}
	refused := false
	for index, entry := range entries {
		item := &items[index]
		item.Entry, item.Plan = index+1, entry
		item.ToPath = entry.ToPath
		item.Problems = validateMoveEntry(entry)
		if len(item.Problems) == 0 {
			if err := loadMoveSource(ctx, tx, from, to, item); err != nil {
				return nil, err
			}
		}
		if item.MediaID != 0 {
			if first, seen := sources[item.MediaID]; seen {
				item.Problems = append(item.Problems, fmt.Sprintf("matches the same media as entry %d", first))
			} else {
				sources[item.MediaID] = item.Entry
			}
		}
		fileKey := fmt.Sprintf("%s/%d", entry.Kind, entry.ToFileID)
		if first, seen := toFiles[fileKey]; seen && entry.ToFileID > 0 {
			item.Problems = append(item.Problems, fmt.Sprintf("to_file_id is also used by entry %d", first))
		} else {
			toFiles[fileKey] = item.Entry
		}
		entityKey := fmt.Sprintf("%s/%d", entry.Kind, entry.ToEntityID)
		if first, seen := toEntities[entityKey]; seen && entry.ToEntityID > 0 {
			item.Problems = append(item.Problems, fmt.Sprintf("to_entity_id is also used by entry %d", first))
		} else {
			toEntities[entityKey] = item.Entry
		}
		if item.MediaID != 0 && len(item.Problems) == 0 && check != nil {
			check(ctx, item)
		}
		refused = refused || len(item.Problems) > 0
	}
	if refused {
		return items, ErrMediaMoveRefused
	}
	if !apply {
		return items, nil
	}
	now := time.Now().UTC().UnixNano()
	for _, item := range items {
		if err := applyMediaMoveTx(ctx, tx, from, to, item, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit media move: %w", err)
	}
	return items, nil
}

func validateMoveEntry(entry MediaMoveEntry) []string {
	var problems []string
	switch entry.Kind {
	case domain.MediaMovie:
		if entry.TMDBID <= 0 && entry.FromFileID <= 0 {
			problems = append(problems, "a movie needs tmdb_id or from_file_id")
		}
		if entry.TVDBID != 0 || entry.Season != 0 || entry.Episode != 0 || entry.ToSeriesID != 0 {
			problems = append(problems, "a movie takes no tvdb_id, season, episode or to_series_id")
		}
	case domain.MediaEpisode:
		identity := entry.TVDBID > 0 && entry.Season >= 0 && entry.Episode > 0
		if !identity && entry.FromFileID <= 0 {
			problems = append(problems, "an episode needs tvdb_id, season and episode, or from_file_id")
		}
		if (entry.TVDBID != 0 || entry.Episode != 0) && !identity {
			problems = append(problems, "tvdb_id, season and episode must be given together")
		}
		if entry.TMDBID != 0 {
			problems = append(problems, "an episode takes no tmdb_id")
		}
		if entry.ToSeriesID <= 0 {
			problems = append(problems, "an episode needs to_series_id")
		}
	default:
		problems = append(problems, "kind must be movie or episode")
	}
	if entry.FromFileID < 0 {
		problems = append(problems, "from_file_id must be positive")
	}
	if entry.ToEntityID <= 0 {
		problems = append(problems, "to_entity_id must be positive")
	}
	if entry.ToFileID <= 0 {
		problems = append(problems, "to_file_id must be positive")
	}
	if !filepath.IsAbs(entry.ToPath) || filepath.Clean(entry.ToPath) != entry.ToPath {
		problems = append(problems, "to_path must be a clean absolute path")
	}
	return problems
}

// loadMoveSource finds the entry's source row under from and reads what the
// move changes, recording problems on item.
func loadMoveSource(ctx context.Context, tx *sql.Tx, from, to string, item *MediaMoveItem) error {
	entry := item.Plan
	query := `SELECT id, file_id, entity_id, path, size, title, year, season, episode FROM media WHERE instance=? AND kind=? AND deleted=0`
	args := []any{from, string(entry.Kind)}
	if entry.FromFileID > 0 {
		query += ` AND file_id=?`
		args = append(args, entry.FromFileID)
	}
	switch {
	case entry.Kind == domain.MediaMovie && entry.TMDBID > 0:
		query += ` AND tmdb_id=?`
		args = append(args, entry.TMDBID)
	case entry.Kind == domain.MediaEpisode && entry.TVDBID > 0:
		query += ` AND tvdb_id=? AND season=? AND episode=?`
		args = append(args, entry.TVDBID, entry.Season, entry.Episode)
	}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY id LIMIT 2`, args...)
	if err != nil {
		return fmt.Errorf("find media to move: %w", err)
	}
	matches := 0
	for rows.Next() {
		matches++
		if err := rows.Scan(&item.MediaID, &item.FromFileID, &item.FromEntityID, &item.FromPath, &item.Size, &item.Title, &item.Year, &item.Season, &item.Episode); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan media to move: %w", err)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("find media to move: %w", err)
	}
	switch matches {
	case 0:
		item.MediaID = 0
		item.Problems = append(item.Problems, fmt.Sprintf("no active %s under %s matches", entry.Kind, from))
		return nil
	case 2:
		item.MediaID = 0
		item.Problems = append(item.Problems, fmt.Sprintf("more than one active %s under %s matches; add from_file_id", entry.Kind, from))
		return nil
	}

	var conflict int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM media WHERE instance=? AND kind=? AND (file_id=? OR entity_id=?)`, to, string(entry.Kind), entry.ToFileID, entry.ToEntityID).Scan(&conflict); err != nil {
		return fmt.Errorf("check media move conflicts: %w", err)
	}
	if conflict > 0 {
		item.Problems = append(item.Problems, fmt.Sprintf("%s already has a %s row (active or deleted) with to_file_id or to_entity_id", to, entry.Kind))
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pending_installs WHERE media_id=?`, item.MediaID).Scan(&pending); err != nil {
		return fmt.Errorf("check pending installs: %w", err)
	}
	if pending > 0 {
		item.Problems = append(item.Problems, "an interrupted install or removal is pending; start subsyncd once to recover it first")
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE next_attempt_at_ns<>0 AND json_extract(payload_json,'$.media.ref.instance')=? AND json_extract(payload_json,'$.media.ref.kind')=? AND json_extract(payload_json,'$.media.ref.file_id')=?`, from, string(entry.Kind), item.FromFileID).Scan(&item.PendingNotifications); err != nil {
		return fmt.Errorf("count pending notifications: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tracks WHERE media_id=? AND embedded=0 AND path<>''`, item.MediaID).Scan(&item.Tracks); err != nil {
		return fmt.Errorf("count external tracks: %w", err)
	}

	installations, err := tx.QueryContext(ctx, `SELECT language, path, checksum, rollback_path, media_path, media_file_id FROM installations WHERE media_id=? ORDER BY language`, item.MediaID)
	if err != nil {
		return fmt.Errorf("list installations to move: %w", err)
	}
	for installations.Next() {
		var sidecar MovedSidecar
		var mediaPath string
		var mediaFileID int64
		if err := installations.Scan(&sidecar.Language, &sidecar.From, &sidecar.Checksum, &sidecar.RollbackFrom, &mediaPath, &mediaFileID); err != nil {
			_ = installations.Close()
			return fmt.Errorf("scan installation to move: %w", err)
		}
		switch {
		case mediaFileID != 0 && mediaFileID != item.FromFileID:
			item.Problems = append(item.Problems, fmt.Sprintf("the %s installation was recorded for an earlier file; start subsyncd once to settle it first", sidecar.Language))
		case mediaPath != "" && filepath.Clean(mediaPath) != filepath.Clean(item.FromPath):
			item.Problems = append(item.Problems, fmt.Sprintf("the %s installation still names an earlier media path; start subsyncd once to settle it first", sidecar.Language))
		case filepath.Dir(filepath.Clean(sidecar.From)) != filepath.Dir(filepath.Clean(item.FromPath)):
			item.Problems = append(item.Problems, fmt.Sprintf("the %s sidecar is not in the media folder", sidecar.Language))
		case sidecar.RollbackFrom != "" && filepath.Dir(filepath.Clean(sidecar.RollbackFrom)) != filepath.Dir(filepath.Clean(item.FromPath)):
			item.Problems = append(item.Problems, fmt.Sprintf("the %s rollback copy is not in the media folder", sidecar.Language))
		}
		sidecar.To = moveSiblingPath(sidecar.From, entry.ToPath)
		if sidecar.RollbackFrom != "" {
			sidecar.RollbackTo = moveSiblingPath(sidecar.RollbackFrom, entry.ToPath)
		}
		item.Sidecars = append(item.Sidecars, sidecar)
	}
	if err := errors.Join(installations.Err(), installations.Close()); err != nil {
		return fmt.Errorf("list installations to move: %w", err)
	}
	return nil
}

// moveSiblingPath names path, a file beside the old media file, in the folder
// of newMediaPath; its name stays.
func moveSiblingPath(path, newMediaPath string) string {
	return filepath.Join(filepath.Dir(newMediaPath), filepath.Base(path))
}

func applyMediaMoveTx(ctx context.Context, tx *sql.Tx, from, to string, item MediaMoveItem, now int64) error {
	entry := item.Plan
	result, err := tx.ExecContext(ctx, `UPDATE media SET instance=?, entity_id=?, file_id=?, series_id=?, path=?, updated_at_ns=? WHERE id=? AND instance=? AND kind=? AND file_id=? AND deleted=0`,
		to, entry.ToEntityID, entry.ToFileID, entry.ToSeriesID, entry.ToPath, now, item.MediaID, from, string(entry.Kind), item.FromFileID)
	if err != nil {
		return fmt.Errorf("move media: %w", err)
	}
	if err := requireOneRow(result, "media changed before it was moved"); err != nil {
		return err
	}
	for _, sidecar := range item.Sidecars {
		result, err := tx.ExecContext(ctx, `UPDATE installations SET path=?, rollback_path=?,
			media_path=CASE WHEN media_path='' THEN '' ELSE ? END,
			media_file_id=CASE WHEN media_file_id=0 THEN 0 ELSE ? END
			WHERE media_id=? AND language=? AND path=? AND checksum=? AND rollback_path=?`,
			sidecar.To, sidecar.RollbackTo, entry.ToPath, entry.ToFileID, item.MediaID, sidecar.Language, sidecar.From, sidecar.Checksum, sidecar.RollbackFrom)
		if err != nil {
			return fmt.Errorf("move installation: %w", err)
		}
		if err := requireOneRow(result, "installation changed before it was moved"); err != nil {
			return err
		}
	}
	oldFolder := filepath.Dir(item.FromPath) + string(filepath.Separator)
	newFolder := filepath.Dir(entry.ToPath) + string(filepath.Separator)
	// External tracks in the media folder follow it; the probe refresh
	// rereads them anyway.
	if _, err := tx.ExecContext(ctx, `UPDATE tracks SET path=?2 || substr(path, length(?1)+1) WHERE media_id=?3 AND embedded=0 AND substr(path, 1, length(?1))=?1 AND instr(substr(path, length(?1)+1), ?4)=0`,
		oldFolder, newFolder, item.MediaID, string(filepath.Separator)); err != nil {
		return fmt.Errorf("move external tracks: %w", err)
	}
	// Evidence recorded for the source file is bound to the same bytes at
	// the new path and file ID.
	for _, statement := range []string{
		`UPDATE media_hashes SET fingerprint_path=?1, fingerprint_file_id=?2 WHERE media_id=?3 AND fingerprint_path=?4 AND fingerprint_file_id=?5`,
		`UPDATE candidate_rejections SET media_path=?1, media_file_id=?2 WHERE media_id=?3 AND media_path=?4 AND media_file_id=?5`,
		`UPDATE candidate_lapse_failures SET media_path=?1, media_file_id=?2 WHERE media_id=?3 AND media_path=?4 AND media_file_id=?5`,
	} {
		if _, err := tx.ExecContext(ctx, statement, entry.ToPath, entry.ToFileID, item.MediaID, item.FromPath, item.FromFileID); err != nil {
			return fmt.Errorf("move media evidence: %w", err)
		}
	}
	// Searches take the target instance's queue rank (startup re-ranks too).
	if _, err := tx.ExecContext(ctx, `UPDATE search_states SET instance_rank=COALESCE((SELECT queue_priority FROM instances WHERE name=?), 0) WHERE media_id=?`, to, item.MediaID); err != nil {
		return fmt.Errorf("re-rank moved searches: %w", err)
	}
	return nil
}

// InstanceType returns the type stored for an instance (radarr or sonarr)
// and whether the database knows it.
func (r *Repository) InstanceType(ctx context.Context, name string) (string, bool, error) {
	var instanceType string
	err := r.store.db.QueryRowContext(ctx, `SELECT type FROM instances WHERE name=?`, name).Scan(&instanceType)
	if errors.Is(err, sql.ErrNoRows) {
		var media int
		if err := r.store.db.QueryRowContext(ctx, `SELECT count(*) FROM media WHERE instance=?`, name).Scan(&media); err != nil {
			return "", false, fmt.Errorf("read instance %s: %w", name, err)
		}
		return "", media > 0, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read instance %s: %w", name, err)
	}
	return instanceType, true, nil
}
