package app

import (
	"context"
	"fmt"

	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/selection"
	"subsyncd/internal/store"
)

// defaultMissingLimit is how many rows missing lists without --limit.
const defaultMissingLimit = 50

// Missing lists the configured languages that active media have no subtitle
// for, in queue order, each with a reason and a command that addresses it. It
// only reads the database and opens no media file; New has already checked
// that the media roots exist, as for every command. A search that is running
// now is listed like the others, with no next attempt.
func (a *App) Missing(ctx context.Context, args control.MissingArgs) (control.MissingResult, error) {
	if args.Instance != "" && a.Catalogs[args.Instance] == nil {
		return control.MissingResult{}, unknownInstance(args.Instance)
	}
	query := store.MissingQuery{Instance: args.Instance, Limit: args.Limit, Languages: sortedLanguages(a.Config), AllowHearingImpaired: a.Config.AllowHearingImpaired, Now: a.Clock.Now()}
	if args.Limit < 0 {
		return control.MissingResult{}, selectionUsage("limit must not be negative")
	}
	if args.Limit == 0 {
		query.Limit = defaultMissingLimit
	}
	if args.Language != "" {
		language, err := domain.ParseLanguage(args.Language)
		if err != nil {
			return control.MissingResult{}, &control.Error{Code: control.CodeUnknownLanguage, Message: err.Error()}
		}
		if _, ok := a.Config.Languages[language]; !ok {
			return control.MissingResult{}, languageNotConfigured(language)
		}
		query.Language = language
	}
	if args.Title != "" {
		title, err := selection.ParseTitle(args.Title)
		if err != nil {
			return control.MissingResult{}, selectionUsage(err.Error())
		}
		query.Title = title
	}
	rows, total, err := a.Repository.ListMissing(ctx, query)
	if err != nil {
		return control.MissingResult{}, err
	}
	health := map[string]string{}
	for _, row := range rows {
		for _, providerID := range a.Config.Languages[domain.Language(row.Language)].AllProviders() {
			if _, known := health[providerID]; known {
				continue
			}
			state, err := a.providerHealth(ctx, providerID, query.Now)
			if err != nil {
				return control.MissingResult{}, err
			}
			health[providerID] = state.State
		}
	}
	result := control.MissingResult{Total: total, Items: make([]control.MissingItem, 0, len(rows))}
	for _, row := range rows {
		item := control.MissingItem{MediaMatch: row.MediaMatch, Language: row.Language}
		if !row.Running {
			item.NextAttemptAt = row.NextAttemptAt
		}
		item.Reason, item.Providers = missingReason(row, a.Config.Languages[domain.Language(row.Language)].AllProviders(), health)
		item.Action = missingAction(item, health)
		result.Items = append(result.Items, item)
	}
	return result, nil
}

// missingReason classifies a row. Unsupported media wins; then a search whose
// last run failed technically (the stored state cannot say why, so a media
// outage is search_failing too); then a throttled last search or a route whose
// providers are all cooling down; then the last search's outcome. Only a
// search that never ran is not_searched.
func missingReason(row store.MissingRow, route []string, health map[string]string) (string, []string) {
	if row.UnsupportedReason != "" {
		return "unsupported", nil
	}
	if row.LastOutcome == "workflow_error" || (row.LastOutcome == "" && row.FailureAttempt > 0) {
		return "search_failing", nil
	}
	var coolingRoute []string
	for _, providerID := range route {
		if state := health[providerID]; state != "" && state != providerAvailable {
			coolingRoute = append(coolingRoute, providerID)
		}
	}
	if row.LastOutcome == "throttled" || (len(route) > 0 && len(coolingRoute) == len(route)) {
		return "providers_cooling_down", coolingRoute
	}
	switch row.LastOutcome {
	case "no_result":
		return "no_candidates", nil
	case "rejected":
		return "all_rejected", nil
	}
	return "not_searched", nil
}

// missingAction names the command that addresses the item. A cooldown or an
// exhausted quota ends by itself at its reset, so the action is providers,
// which shows when; only rejected credentials need retry, after fixing them.
func missingAction(item control.MissingItem, health map[string]string) string {
	search := fmt.Sprintf("subsyncd search --instance %s --kind %s --file-id %d --language %s", item.Instance, item.Kind, item.FileID, item.Language)
	switch item.Reason {
	case "all_rejected":
		return search + " --retry-rejected"
	case "providers_cooling_down":
		for _, providerID := range item.Providers {
			if health[providerID] == providerAuthFailed {
				return "subsyncd retry --provider " + providerID
			}
		}
		if len(item.Providers) > 0 {
			return "subsyncd providers"
		}
		return search
	case "search_failing":
		return fmt.Sprintf("subsyncd explain --instance %s --kind %s --file-id %d --language %s", item.Instance, item.Kind, item.FileID, item.Language)
	case "unsupported":
		return "replace the file in Sonarr or Radarr with one subsyncd supports (for example one episode per file); subsyncd does not search it"
	}
	return search
}
