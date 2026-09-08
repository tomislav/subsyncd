package catalog

import (
	"context"
	"fmt"
	"time"

	"subsyncd/internal/domain"
)

type libraryDiscoveryStore interface {
	LibraryDiscoveryState(context.Context, string) (string, int64, error)
	CommitLibraryDiscovery(context.Context, string, string, int64, []domain.Media, []domain.Language, time.Time) error
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
	items, err := source.ListLibrary(ctx)
	if err != nil {
		return fmt.Errorf("enumerate library: %w", err)
	}
	if err := repository.CommitLibraryDiscovery(ctx, r.Instance, r.LibraryScope, revision, items, r.Languages, r.Now().UTC()); err != nil {
		return fmt.Errorf("commit library discovery: %w", err)
	}
	if r.OnCommitted != nil {
		r.OnCommitted()
	}
	return nil
}
