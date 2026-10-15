package app

import (
	"context"
	"sort"

	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

// Coverage counts, for each instance and configured language, the active
// media files and how many are installed, covered by an existing track or
// missing, with the missing ones split by missing's reasons. Missing is
// exactly what the missing command lists, and existing is the rest, so the
// three always add up to the media count; they come from one database
// snapshot. Configured instances come first, in configuration order, then
// any instance still in the database but no longer configured (until
// forget-instance removes it), by name. It only reads the database.
func (a *App) Coverage(ctx context.Context, args control.CoverageArgs) (control.CoverageResult, error) {
	if args.Instance != "" && a.Catalogs[args.Instance] == nil {
		return control.CoverageResult{}, unknownInstance(args.Instance)
	}
	query := store.MissingQuery{Instance: args.Instance, Languages: sortedLanguages(a.Config), AllowHearingImpaired: a.Config.AllowHearingImpaired, Now: a.Clock.Now()}
	if args.Language != "" {
		language, err := a.configuredLanguage(args.Language)
		if err != nil {
			return control.CoverageResult{}, err
		}
		query.Language = language
	}
	snapshot, err := a.Repository.Coverage(ctx, query)
	if err != nil {
		return control.CoverageResult{}, err
	}
	items, err := a.classifyMissing(ctx, snapshot.Missing, query.Now)
	if err != nil {
		return control.CoverageResult{}, err
	}
	reasons := map[store.InstanceLanguage][]control.ReasonCount{}
	for _, group := range summarizeMissing(items) {
		key := store.InstanceLanguage{Instance: group.Instance, Language: group.Language}
		reasons[key] = append(reasons[key], control.ReasonCount{Reason: group.Reason, Count: group.Count})
	}
	languages := query.Languages
	if query.Language != "" {
		languages = []domain.Language{query.Language}
	}
	type instanceRow struct {
		name       string
		configured bool
	}
	var instances []instanceRow
	configured := map[string]bool{}
	for _, instance := range a.Config.Instances {
		configured[instance.Name] = true
		if args.Instance == "" || instance.Name == args.Instance {
			instances = append(instances, instanceRow{instance.Name, true})
		}
	}
	if args.Instance == "" {
		var stale []string
		for name := range snapshot.Media {
			if !configured[name] {
				stale = append(stale, name)
			}
		}
		sort.Strings(stale)
		for _, name := range stale {
			instances = append(instances, instanceRow{name, false})
		}
	}
	result := control.CoverageResult{Rows: []control.CoverageRow{}}
	for _, instance := range instances {
		for _, language := range languages {
			key := store.InstanceLanguage{Instance: instance.name, Language: language.String()}
			row := control.CoverageRow{Instance: instance.name, Configured: instance.configured, Language: language.String(), Media: snapshot.Media[instance.name], Installed: snapshot.Installed[key], MissingByReason: []control.ReasonCount{}}
			for _, reason := range reasons[key] {
				row.MissingByReason = append(row.MissingByReason, reason)
				row.Missing += reason.Count
			}
			row.Covered = row.Media - row.Missing
			row.Existing = row.Covered - row.Installed
			result.Rows = append(result.Rows, row)
		}
	}
	return result, nil
}
