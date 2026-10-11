package store

import (
	"context"
	"errors"
	"fmt"
)

// ErrUnknownInstance reports that the database holds nothing for an instance.
var ErrUnknownInstance = errors.New("instance is not in the database")

// ForgetInstanceResult counts the rows removed (or, for a preview, the rows
// that would be removed) for one instance. RollbackCopies counts the
// installations whose hidden rollback copy stays on disk untracked.
type ForgetInstanceResult struct {
	Media          int64
	Searches       int64
	Installations  int64
	RollbackCopies int64
	Candidates     int64
	Rejections     int64
	Notifications  int64
	Events         int64
	Replays        int64
}

// ForgetInstance deletes every row owned by a removed Arr instance in one
// transaction: its media (and, by cascade, tracks, probes, hashes, searches,
// candidates, rejections, LAPSE failures and installation records), its audit
// events, reconciliation replays, outbox notifications and the instance row.
// Subtitle files on disk are not touched. With apply false nothing is
// committed and the result is a preview.
func (r *Repository) ForgetInstance(ctx context.Context, name string, apply bool) (ForgetInstanceResult, error) {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return ForgetInstanceResult{}, fmt.Errorf("begin forget instance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Every child table but events relies on ON DELETE CASCADE; with foreign
	// keys off the media delete would succeed and orphan them.
	var foreignKeys int
	if err := tx.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return ForgetInstanceResult{}, fmt.Errorf("check foreign keys: %w", err)
	}
	if foreignKeys != 1 {
		return ForgetInstanceResult{}, errors.New("forget instance requires SQLite foreign key enforcement")
	}

	var known int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM instances WHERE name=?1) OR EXISTS(SELECT 1 FROM media WHERE instance=?1)`, name).Scan(&known); err != nil {
		return ForgetInstanceResult{}, fmt.Errorf("check forgotten instance: %w", err)
	}
	if known == 0 {
		return ForgetInstanceResult{}, ErrUnknownInstance
	}

	const ownedMedia = `SELECT id FROM media WHERE instance=?1`
	var result ForgetInstanceResult
	for _, count := range []struct {
		dest  *int64
		query string
	}{
		{&result.Media, `SELECT count(*) FROM media WHERE instance=?1`},
		{&result.Searches, `SELECT count(*) FROM search_states WHERE media_id IN (` + ownedMedia + `)`},
		{&result.Installations, `SELECT count(*) FROM installations WHERE media_id IN (` + ownedMedia + `)`},
		{&result.RollbackCopies, `SELECT count(*) FROM installations WHERE rollback_path<>'' AND media_id IN (` + ownedMedia + `)`},
		{&result.Candidates, `SELECT count(*) FROM candidates WHERE media_id IN (` + ownedMedia + `)`},
		{&result.Rejections, `SELECT count(*) FROM candidate_rejections WHERE media_id IN (` + ownedMedia + `)`},
		{&result.Notifications, `SELECT count(*) FROM notifications WHERE json_extract(payload_json,'$.media.ref.instance')=?1`},
		{&result.Events, `SELECT count(*) FROM events WHERE instance=?1 OR media_id IN (` + ownedMedia + `)`},
		{&result.Replays, `SELECT count(*) FROM reconciliation_replays WHERE instance=?1`},
	} {
		if err := tx.QueryRowContext(ctx, count.query, name).Scan(count.dest); err != nil {
			return ForgetInstanceResult{}, fmt.Errorf("count forgotten instance rows: %w", err)
		}
	}
	if !apply {
		return result, nil
	}

	// Events go before media: deleting media would otherwise detach their
	// media_id and leave unattributed audit rows behind.
	for _, statement := range []string{
		`DELETE FROM notifications WHERE json_extract(payload_json,'$.media.ref.instance')=?1`,
		`DELETE FROM events WHERE instance=?1 OR media_id IN (` + ownedMedia + `)`,
		`DELETE FROM reconciliation_replays WHERE instance=?1`,
		`DELETE FROM media WHERE instance=?1`,
		`DELETE FROM instances WHERE name=?1`,
	} {
		if _, err := tx.ExecContext(ctx, statement, name); err != nil {
			return ForgetInstanceResult{}, fmt.Errorf("forget instance rows: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ForgetInstanceResult{}, fmt.Errorf("commit forget instance: %w", err)
	}
	return result, nil
}
