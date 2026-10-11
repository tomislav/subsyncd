package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// PendingInstall records a subtitle install from just before its file is
// published until the installation is recorded. A row that outlives its
// process marks a crash in between; startup undoes that publication.
type PendingInstall struct {
	ID           int64
	MediaID      int64
	Language     string
	Destination  string
	Checksum     string
	StagedPath   string
	RollbackPath string
	CreatedAt    time.Time
}

func (r *Repository) BeginPendingInstall(ctx context.Context, pending PendingInstall) (int64, error) {
	if pending.MediaID <= 0 || pending.Language == "" || pending.Destination == "" || pending.Checksum == "" {
		return 0, fmt.Errorf("pending install media, language, destination and checksum are required")
	}
	if pending.CreatedAt.IsZero() {
		pending.CreatedAt = time.Now()
	}
	result, err := r.store.db.ExecContext(ctx, `INSERT INTO pending_installs(media_id, language, destination, checksum, staged_path, rollback_path, created_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		pending.MediaID, pending.Language, pending.Destination, pending.Checksum, pending.StagedPath, pending.RollbackPath, pending.CreatedAt.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("record pending install: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read pending install ID: %w", err)
	}
	return id, nil
}

func (r *Repository) FinishPendingInstall(ctx context.Context, id int64) error {
	if _, err := r.store.db.ExecContext(ctx, `DELETE FROM pending_installs WHERE id=?`, id); err != nil {
		return fmt.Errorf("clear pending install: %w", err)
	}
	return nil
}

func (r *Repository) ListPendingInstalls(ctx context.Context) ([]PendingInstall, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT id, media_id, language, destination, checksum, staged_path, rollback_path, created_at_ns FROM pending_installs ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list pending installs: %w", err)
	}
	var pending []PendingInstall
	for rows.Next() {
		var item PendingInstall
		var created int64
		if err := rows.Scan(&item.ID, &item.MediaID, &item.Language, &item.Destination, &item.Checksum, &item.StagedPath, &item.RollbackPath, &created); err != nil {
			return nil, errors.Join(fmt.Errorf("scan pending install: %w", err), rows.Close())
		}
		item.CreatedAt = time.Unix(0, created).UTC()
		pending = append(pending, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list pending installs: %w", err)
	}
	return pending, nil
}
