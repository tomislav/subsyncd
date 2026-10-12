package store

import (
	"context"
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
	args := append([]any{path, mediaPath}, installationGuardArgs(installation)...)
	args = append(args, installation.MediaPath)
	result, err := r.store.db.ExecContext(ctx, `UPDATE installations SET path=?, media_path=? WHERE `+installationGuard+` AND media_path=?`, args...)
	if err != nil {
		return fmt.Errorf("rebase installation: %w", err)
	}
	return requireOneRow(result, "installation changed before it was rebased")
}
