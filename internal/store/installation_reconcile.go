package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// installationGuard matches an installation row only while it is exactly the
// record read: the same sidecar path, checksum, candidate and rollback copy.
const installationGuard = `media_id=? AND language=? AND path=? AND checksum=? AND provider_id=? AND candidate_id=? AND rollback_path=?`

func installationGuardArgs(installation Installation) []any {
	return []any{installation.MediaID, installation.Language, installation.Path, installation.Checksum, installation.ProviderID, installation.CandidateID, installation.RollbackPath}
}

// DropInstallation deletes a stale installation record without touching any
// file: its sidecar is gone, or is no longer the subtitle subsyncd installed
// for the current media file. It records a subtitle_record_dropped audit event
// whose outcome is reason. It fails, changing nothing, when the record is no
// longer the one read.
func (r *Repository) DropInstallation(ctx context.Context, installation Installation, reason string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin installation drop: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `DELETE FROM installations WHERE `+installationGuard, installationGuardArgs(installation)...)
	if err != nil {
		return fmt.Errorf("drop installation: %w", err)
	}
	if err := requireOneRow(result, "installation changed before it was dropped"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(event_type, media_id, outcome, created_at_ns) VALUES ('subtitle_record_dropped', ?, ?, ?)`, installation.MediaID, reason, at.UnixNano()); err != nil {
		return fmt.Errorf("record installation drop audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit installation drop: %w", err)
	}
	return nil
}

// RebaseInstallation records where an installation's sidecar is now (path)
// and which media path it is named after (mediaPath), after the workflow has
// checked the file or moved it there. Everything else, including the recorded
// media file ID and size, stays. It fails, changing nothing, when the record
// is no longer the one read.
func (r *Repository) RebaseInstallation(ctx context.Context, installation Installation, path, mediaPath string) error {
	_, err := r.RebaseInstallationWithNotifications(ctx, installation, path, mediaPath, nil)
	return err
}

// RebaseInstallationWithNotifications is RebaseInstallation that also queues
// notification intents in the same transaction, for a sidecar the workflow
// moved on disk.
func (r *Repository) RebaseInstallationWithNotifications(ctx context.Context, installation Installation, path, mediaPath string, requests []NotificationRequest) ([]NotificationEnqueueResult, error) {
	for _, request := range requests {
		if err := validateNotificationRequest(request); err != nil {
			return nil, err
		}
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin installation rebase: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	args := append([]any{path, mediaPath}, installationGuardArgs(installation)...)
	args = append(args, installation.MediaPath)
	result, err := tx.ExecContext(ctx, `UPDATE installations SET path=?, media_path=? WHERE `+installationGuard+` AND media_path=?`, args...)
	if err != nil {
		return nil, fmt.Errorf("rebase installation: %w", err)
	}
	if err := requireOneRow(result, "installation changed before it was rebased"); err != nil {
		return nil, err
	}
	results := make([]NotificationEnqueueResult, 0, len(requests))
	for _, request := range requests {
		inserted, err := enqueueNotification(ctx, tx, request)
		if err != nil {
			return nil, err
		}
		results = append(results, NotificationEnqueueResult{Notifier: request.Notifier, DedupeKey: request.DedupeKey, Inserted: inserted})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit installation rebase: %w", err)
	}
	return results, nil
}

// RollbackCopy names an installation's hidden rollback copy and the sidecar
// it sits beside.
type RollbackCopy struct {
	MediaID      int64
	Language     string
	Path         string
	RollbackPath string
}

// ListRollbackCopies lists the installations that still record a rollback
// copy.
func (r *Repository) ListRollbackCopies(ctx context.Context) ([]RollbackCopy, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT media_id, language, path, rollback_path FROM installations WHERE rollback_path<>'' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list rollback copies: %w", err)
	}
	var copies []RollbackCopy
	for rows.Next() {
		var item RollbackCopy
		if err := rows.Scan(&item.MediaID, &item.Language, &item.Path, &item.RollbackPath); err != nil {
			return nil, errors.Join(fmt.Errorf("scan rollback copy: %w", err), rows.Close())
		}
		copies = append(copies, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list rollback copies: %w", err)
	}
	return copies, nil
}

// ClearInstallationRollback forgets an installation's rollback copy once it
// is deleted. A record that no longer names that copy (replaced or removed
// meanwhile) is left as it is.
func (r *Repository) ClearInstallationRollback(ctx context.Context, mediaID int64, language, rollbackPath string) error {
	if rollbackPath == "" {
		return nil
	}
	if _, err := r.store.db.ExecContext(ctx, `UPDATE installations SET rollback_path='' WHERE media_id=? AND language=? AND rollback_path=?`, mediaID, language, rollbackPath); err != nil {
		return fmt.Errorf("clear installation rollback copy: %w", err)
	}
	return nil
}
