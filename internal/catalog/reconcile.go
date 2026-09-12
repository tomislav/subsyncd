package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

type ReconciliationStore interface {
	GetReconciliationState(context.Context, string) (store.ReconciliationState, error)
	CommitReconciliation(context.Context, string, store.ReconciliationState, time.Time, []store.MediaEventMutation) error
}

type Reconciler struct {
	Instance     string
	LibraryScope string
	Catalog      Catalog
	Store        ReconciliationStore
	Languages    []domain.Language
	Now          func() time.Time
	OnCommitted  func()
}

func (r Reconciler) Run(ctx context.Context) error {
	if err := r.DiscoverLibrary(ctx, false); err != nil {
		return err
	}
	snapshot, err := r.Store.GetReconciliationState(ctx, r.Instance)
	if err != nil {
		return fmt.Errorf("read %s reconciliation state: %w", r.Instance, err)
	}
	pageEnd := r.Now().UTC()
	changes, listErr := r.Catalog.ListChanges(ctx, snapshot.Cursor, pageEnd)
	if listErr != nil && !errors.Is(listErr, ErrHistoryDeferred) {
		return fmt.Errorf("list %s history since %s: %w", r.Instance, snapshot.Cursor, listErr)
	}
	mutations := make([]store.MediaEventMutation, 0, len(changes))
	for _, change := range changes {
		if change.HistoryID <= 0 || change.EntityID <= 0 || (change.Kind != domain.MediaMovie && change.Kind != domain.MediaEpisode) || change.OccurredAt.IsZero() {
			return fmt.Errorf("%s reconciliation history identity is incomplete", r.Instance)
		}
		ref := domain.MediaRef{Instance: r.Instance, Kind: change.Kind}
		mutationType := EventDelete
		switch change.State {
		case HistoryPresent:
			if change.Type != EventImport && change.Type != EventRename {
				return fmt.Errorf("%s present reconciliation history has invalid event type %q", r.Instance, change.Type)
			}
			if change.Media.EntityID != change.EntityID || change.Media.Ref.Instance != r.Instance || change.Media.Ref.Kind != change.Kind || change.Media.Ref.FileID <= 0 {
				return fmt.Errorf("%s present reconciliation history does not match hydrated media", r.Instance)
			}
			ref = change.Media.Ref
			mutationType = change.Type
		case HistoryAbsent, HistoryOutsideScope:
			mutationType = EventDelete
		default:
			return fmt.Errorf("%s reconciliation history has invalid state %q", r.Instance, change.State)
		}
		mutations = append(mutations, store.MediaEventMutation{
			EventID:   fmt.Sprintf("reconcile:%s:%d", r.Instance, change.HistoryID),
			Type:      string(mutationType),
			EntityID:  change.EntityID,
			Media:     change.Media,
			Ref:       ref,
			Languages: r.Languages,
			At:        change.OccurredAt,
			Priority:  store.SearchPriorityMissing,
		})
	}
	commitCursor := pageEnd
	if listErr != nil {
		commitCursor = snapshot.Cursor
	}
	if err := r.Store.CommitReconciliation(ctx, r.Instance, snapshot, commitCursor, mutations); err != nil {
		return fmt.Errorf("commit %s reconciliation page: %w", r.Instance, err)
	}
	if r.OnCommitted != nil {
		r.OnCommitted()
	}
	if listErr != nil {
		return fmt.Errorf("list %s history since %s: %w", r.Instance, snapshot.Cursor, listErr)
	}
	return nil
}
