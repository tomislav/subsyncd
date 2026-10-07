package catalog

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

type ReconciliationStore interface {
	GetReconciliationState(context.Context, string) (store.ReconciliationState, error)
	ListActiveCatalogIdentities(context.Context, string, domain.MediaKind) ([]int64, error)
	CommitReconciliation(context.Context, string, store.ReconciliationState, time.Time, []store.MediaEventMutation) error
}

type Reconciler struct {
	Instance     string
	Kind         domain.MediaKind
	LibraryScope string
	Catalog      ReconciliationCatalog
	Store        ReconciliationStore
	Languages    []domain.Language
	Now          func() time.Time
	OnCommitted  func()
	// Events receives background warnings that do not fail reconciliation.
	Events *observability.Emitter
}

func (r Reconciler) Run(ctx context.Context) error {
	if r.Kind != domain.MediaEpisode && r.Kind != domain.MediaMovie {
		return fmt.Errorf("%s reconciler media kind is invalid", r.Instance)
	}
	// A deferred discovery has committed its readable files; history still
	// reconciles, and both deferrals are reported together below.
	discoveryErr := r.DiscoverLibrary(ctx, false)
	if discoveryErr != nil && !errors.Is(discoveryErr, ErrHistoryDeferred) {
		return discoveryErr
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
	mutations = append(mutations, r.recheckLegacyMultiEpisode(ctx)...)
	identitySnapshot, err := r.Catalog.ListIdentitySnapshot(ctx)
	if err != nil {
		return fmt.Errorf("list %s catalog identities: %w", r.Instance, err)
	}
	if identitySnapshot.IDs == nil || identitySnapshot.Kind != r.Kind {
		return fmt.Errorf("%s catalog identity snapshot is invalid", r.Instance)
	}
	for identity := range identitySnapshot.IDs {
		if identity <= 0 {
			return fmt.Errorf("%s catalog identity snapshot is invalid", r.Instance)
		}
	}
	activeIdentities, err := r.Store.ListActiveCatalogIdentities(ctx, r.Instance, identitySnapshot.Kind)
	if err != nil {
		return fmt.Errorf("list %s active catalog identities: %w", r.Instance, err)
	}
	sort.Slice(activeIdentities, func(i, j int) bool { return activeIdentities[i] < activeIdentities[j] })
	for _, identity := range activeIdentities {
		if identity <= 0 {
			return fmt.Errorf("%s active catalog identity is invalid", r.Instance)
		}
		if _, present := identitySnapshot.IDs[identity]; present {
			continue
		}
		mutation := store.MediaEventMutation{
			EventID:  snapshotDeleteEventID(r.Instance, identitySnapshot.Kind, identity, pageEnd),
			Type:     string(EventDelete),
			Ref:      domain.MediaRef{Instance: r.Instance, Kind: identitySnapshot.Kind},
			At:       pageEnd,
			Priority: store.SearchPriorityMissing,
		}
		if identitySnapshot.Kind == domain.MediaEpisode {
			mutation.SeriesID = identity
		} else {
			mutation.EntityID = identity
		}
		mutations = append(mutations, mutation)
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
	if listErr != nil || discoveryErr != nil {
		return fmt.Errorf("%s reconciliation deferred (history since %s): %w", r.Instance, snapshot.Cursor, mergeDeferrals(discoveryErr, listErr))
	}
	return nil
}

func snapshotDeleteEventID(instance string, kind domain.MediaKind, identity int64, pageEnd time.Time) string {
	input := fmt.Sprintf("%d:%s:%s:%d:%s", len(instance), instance, kind, identity, pageEnd.UTC().Format(time.RFC3339Nano))
	return fmt.Sprintf("snapshot-reconcile:%x", sha256.Sum256([]byte(input)))
}

type legacyMultiEpisodeStore interface {
	ListLegacyMultiEpisodeMedia(context.Context, string) ([]domain.MediaRef, error)
}

// recheckLegacyMultiEpisode re-hydrates, once, files marked unsupported
// multi-episode before episode ranges were supported. Each result is committed
// as an import, which stores either the range (and schedules its searches) or
// the checked-unsupported marker. A file that fails to hydrate is skipped and
// retried on the next reconciliation.
func (r Reconciler) recheckLegacyMultiEpisode(ctx context.Context) []store.MediaEventMutation {
	repository, ok := r.Store.(legacyMultiEpisodeStore)
	if r.Kind != domain.MediaEpisode || !ok {
		return nil
	}
	events := r.Events.For("catalog")
	refs, err := repository.ListLegacyMultiEpisodeMedia(ctx, r.Instance)
	if err != nil {
		events.Log(ctx, slog.LevelWarn, "reconcile.multi_episode_recheck_failed", "could not list legacy multi-episode files",
			append([]slog.Attr{slog.String("instance", r.Instance)}, events.ErrorAttrs("list", err)...)...)
		return nil
	}
	now := r.Now().UTC()
	var mutations []store.MediaEventMutation
	for _, ref := range refs {
		if ctx.Err() != nil {
			return mutations
		}
		media, err := r.Catalog.GetMedia(ctx, ref)
		if err == nil && (media.EntityID <= 0 || media.Ref != ref) {
			err = fmt.Errorf("hydrated media does not match the file")
		}
		if err != nil {
			// Retried on the next reconciliation (every six hours).
			events.Log(ctx, slog.LevelWarn, "reconcile.multi_episode_recheck_failed", "could not re-check a multi-episode file",
				append([]slog.Attr{slog.String("instance", r.Instance), slog.Int64("file_id", ref.FileID)}, events.ErrorAttrs("hydration", err)...)...)
			continue
		}
		mutations = append(mutations, store.MediaEventMutation{
			// Stable per file, so a retried commit applies the re-check once.
			EventID:   fmt.Sprintf("multi-episode-recheck:%s:%d", r.Instance, ref.FileID),
			Type:      string(EventImport),
			EntityID:  media.EntityID,
			Media:     media,
			Ref:       ref,
			Languages: r.Languages,
			At:        now,
			Priority:  store.SearchPriorityMissing,
		})
	}
	return mutations
}
