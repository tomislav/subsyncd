package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"subsyncd/internal/control"
	"subsyncd/internal/store"
)

const (
	defaultRecentWindow = 24 * time.Hour
	defaultRecentLimit  = 50
)

// Recent lists finished searches and user rejections in a time window, newest
// first. It only reads the search history, which outlives the media rows it
// describes, so a removed file still appears under its stored title.
func (a *App) Recent(ctx context.Context, args control.RecentArgs) (control.RecentResult, error) {
	if args.Since < 0 {
		return control.RecentResult{}, selectionUsage("since must be positive")
	}
	if args.Limit < 0 {
		return control.RecentResult{}, selectionUsage("limit must not be negative")
	}
	if args.Instance != "" && a.Catalogs[args.Instance] == nil {
		return control.RecentResult{}, unknownInstance(args.Instance)
	}
	for _, outcome := range args.Outcomes {
		if !control.ValidRecentOutcome(outcome) {
			return control.RecentResult{}, selectionUsage(fmt.Sprintf("unknown outcome %q; use %s", outcome, strings.Join(control.RecentOutcomes, ", ")))
		}
	}
	window, limit := args.Since, args.Limit
	if window == 0 {
		window = defaultRecentWindow
	}
	if limit == 0 {
		limit = defaultRecentLimit
	}
	query := store.HistoryQuery{Since: a.Clock.Now().Add(-window), Instance: args.Instance, Outcomes: args.Outcomes, Limit: limit}
	if args.Language != "" {
		language, err := a.configuredLanguage(args.Language)
		if err != nil {
			return control.RecentResult{}, err
		}
		query.Language = language
	}
	entries, err := a.Repository.ListHistory(ctx, query)
	if err != nil {
		return control.RecentResult{}, err
	}
	result := control.RecentResult{Since: query.Since, Items: make([]control.RecentItem, 0, len(entries))}
	for _, entry := range entries {
		result.Items = append(result.Items, control.RecentItem{
			MediaMatch: control.MediaMatch{Instance: entry.Ref.Instance, Kind: string(entry.Ref.Kind), FileID: entry.Ref.FileID, Title: entry.MediaTitle},
			Language:   entry.Language.String(), Outcome: entry.Outcome, Provider: entry.ProviderID, Release: entry.ReleaseName,
			Reason: entry.Reason, Manual: entry.Manual, FinishedAt: entry.FinishedAt,
		})
	}
	return result, nil
}
