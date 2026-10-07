package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"subsyncd/internal/domain"
)

type libraryDiscoveryStore interface {
	LibraryDiscoveryState(context.Context, string) (string, int64, error)
	CommitLibraryDiscovery(context.Context, string, string, int64, []domain.Media, []domain.Language, time.Time) error
	CommitPartialLibraryDiscovery(context.Context, string, int64, []domain.Media, []domain.Language, time.Time) error
}

// DiscoverLibrary fills catalog gaps independently of the history cursor.
// Injected catalogs may implement only history; concrete Arr adapters also
// implement full enumeration. Assembly and diagnostics never invoke this.
func (r Reconciler) DiscoverLibrary(ctx context.Context, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.LibraryScope == "" {
		return nil
	}
	source, ok := r.Catalog.(LibraryCatalog)
	if !ok {
		return nil
	}
	repository, ok := r.Store.(libraryDiscoveryStore)
	if !ok {
		return fmt.Errorf("library discovery persistence unavailable")
	}
	scope, revision, err := repository.LibraryDiscoveryState(ctx, r.Instance)
	if err != nil {
		return fmt.Errorf("read library discovery state: %w", err)
	}
	if !force && scope == r.LibraryScope {
		return nil
	}
	items, listErr := source.ListLibrary(ctx)
	if listErr != nil && !errors.Is(listErr, ErrHistoryDeferred) {
		return fmt.Errorf("enumerate library: %w", listErr)
	}
	// Unreadable files leave discovery incomplete, so a mount outage cannot
	// mark files it skipped as discovered; the readable part commits now.
	if listErr != nil {
		err = repository.CommitPartialLibraryDiscovery(ctx, r.Instance, revision, items, r.Languages, r.Now().UTC())
	} else {
		err = repository.CommitLibraryDiscovery(ctx, r.Instance, r.LibraryScope, revision, items, r.Languages, r.Now().UTC())
	}
	if err != nil {
		return fmt.Errorf("commit library discovery: %w", err)
	}
	if r.OnCommitted != nil {
		r.OnCommitted()
	}
	if listErr != nil {
		return fmt.Errorf("enumerate library: %w", listErr)
	}
	return nil
}
