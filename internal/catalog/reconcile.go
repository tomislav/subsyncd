package catalog

import (
	"context"
	"fmt"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

type ReconciliationStore interface {
	GetReconciliationCursor(context.Context, string) (time.Time, error)
	CommitReconciliation(context.Context, string, time.Time, []store.MediaEventMutation) error
}

type Reconciler struct {
	Instance    string
	Catalog     Catalog
	Store       ReconciliationStore
	Languages   []domain.Language
	Now         func() time.Time
	OnCommitted func()
}

func (r Reconciler) Run(ctx context.Context) error {
	cursor, err := r.Store.GetReconciliationCursor(ctx, r.Instance)
	if err != nil {
		return fmt.Errorf("read %s reconciliation cursor: %w", r.Instance, err)
	}
	pageEnd := r.Now().UTC()
	changes, err := r.Catalog.ListChangesSince(ctx, cursor)
	if err != nil {
		return fmt.Errorf("list %s history since %s: %w", r.Instance, cursor, err)
	}
	mutations := make([]store.MediaEventMutation, 0, len(changes))
	for _, change := range changes {
		mutations = append(mutations, store.MediaEventMutation{
			EventID:   fmt.Sprintf("reconcile:%s:%d", r.Instance, change.HistoryID),
			Type:      string(change.Type),
			EntityID:  change.Media.EntityID,
			Media:     change.Media,
			Ref:       change.Ref,
			Languages: r.Languages,
			At:        change.OccurredAt,
			Priority:  store.SearchPriorityMissing,
		})
	}
	if err := r.Store.CommitReconciliation(ctx, r.Instance, pageEnd, mutations); err != nil {
		return fmt.Errorf("commit %s reconciliation page: %w", r.Instance, err)
	}
	if r.OnCommitted != nil {
		r.OnCommitted()
	}
	return nil
}
