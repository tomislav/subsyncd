package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"subsyncd/internal/domain"
)

// ErrLibraryDiscoveryStale means an event committed while the library was fetched.
var ErrLibraryDiscoveryStale = errors.New("library changed during discovery")

// LibraryDiscoveryState returns completion scope and the event snapshot fence.
func (r *Repository) LibraryDiscoveryState(ctx context.Context, instance string) (scope string, revision int64, err error) {
	err = r.store.db.QueryRowContext(ctx, `SELECT library_scope, event_revision FROM instances WHERE name=?`, instance).Scan(&scope, &revision)
	if err != nil {
		err = fmt.Errorf("read library discovery state: %w", err)
	}
	return
}

// CommitLibraryDiscovery adds only previously unknown media. Existing entity rows,
// including retained deletions and replaced file identities, belong to reconciliation.
func (r *Repository) CommitLibraryDiscovery(ctx context.Context, instance, scope string, revision int64, items []domain.Media, languages []domain.Language, at time.Time) error {
	if strings.TrimSpace(instance) == "" || strings.TrimSpace(scope) == "" || revision < 0 || at.IsZero() {
		return fmt.Errorf("invalid library discovery identity")
	}
	for _, language := range languages {
		canonical, err := domain.ParseLanguage(language.String())
		if err != nil || canonical != language {
			return fmt.Errorf("invalid library discovery language")
		}
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin library discovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var current int64
	var instanceType string
	if err := tx.QueryRowContext(ctx, `SELECT event_revision, type FROM instances WHERE name=?`, instance).Scan(&current, &instanceType); err != nil {
		return fmt.Errorf("read library discovery fence: %w", err)
	}
	if current != revision {
		return ErrLibraryDiscoveryStale
	}
	kind := domain.MediaEpisode
	if instanceType == "radarr" {
		kind = domain.MediaMovie
	} else if instanceType != "sonarr" {
		return fmt.Errorf("invalid library discovery instance type")
	}
	snapshotFiles := make(map[int64]int64, len(items))
	snapshotEntities := make(map[int64]int64, len(items))
	for _, media := range items {
		if media.Ref.Instance != instance || media.Ref.Kind != kind || media.EntityID <= 0 || media.Ref.FileID <= 0 || media.Fingerprint.FileID != media.Ref.FileID || !validUnsupportedReason(media.UnsupportedReason) {
			return fmt.Errorf("invalid library discovery media identity")
		}
		if fileID, seen := snapshotFiles[media.EntityID]; seen && fileID != media.Ref.FileID {
			return fmt.Errorf("conflicting library discovery snapshot identities")
		}
		if entityID, seen := snapshotEntities[media.Ref.FileID]; seen && entityID != media.EntityID {
			return fmt.Errorf("conflicting library discovery snapshot identities")
		}
		snapshotEntities[media.Ref.FileID] = media.EntityID
		snapshotFiles[media.EntityID] = media.Ref.FileID
		_, _, _, _, _, found, err := findMediaIdentityTx(ctx, tx, media)
		if err != nil {
			return err
		}
		if found {
			continue
		}
		identity := fmt.Sprintf("%s\x00%s\x00%d\x00%d", instance, media.Ref.Kind, media.EntityID, media.Ref.FileID)
		mutation := MediaEventMutation{EventID: fmt.Sprintf("library:%x", sha256.Sum256([]byte(identity))), Type: "import", Ref: media.Ref, EntityID: media.EntityID, Media: media, Languages: languages, At: at, Priority: SearchPriorityMissing}
		applied, err := applyMediaMutationTx(ctx, tx, mutation)
		if err != nil {
			return err
		}
		if !applied {
			return fmt.Errorf("library discovery event conflicts with missing media")
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id IN (SELECT id FROM events ORDER BY created_at_ns DESC, id DESC LIMIT -1 OFFSET 10000)`); err != nil {
		return fmt.Errorf("bound media audit log: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE instances SET library_scope=?, updated_at_ns=? WHERE name=?`, scope, at.UnixNano(), instance); err != nil {
		return fmt.Errorf("complete library discovery: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit library discovery: %w", err)
	}
	return nil
}
