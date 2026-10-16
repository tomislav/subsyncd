package app

import (
	"context"
	"sort"
	"time"

	"subsyncd/internal/control"
	"subsyncd/internal/store"
	"subsyncd/internal/worker"
)

// providerHistoryWindow is the span the providers command counts outcomes over.
const providerHistoryWindow = 24 * time.Hour

// statusLive is what the daemon alone knows: its version and start, and the
// worker's media pause. It reads nothing from the database or the network.
func (a *App) statusLive() control.StatusResult {
	result := control.StatusResult{Daemon: control.DaemonState{Running: true, Version: a.Version, StartedAt: a.startedAt}}
	if source, ok := a.Worker.(interface{ Snapshot() worker.Snapshot }); ok {
		if until := source.Snapshot().MediaPausedUntil; until.After(a.Clock.Now()) {
			result.MediaPause = &control.MediaPause{Until: until.UTC()}
		}
	}
	return result
}

// Status reads the overview from the database alone, so it runs beside the
// daemon or without it. Daemon and MediaPause stay unset: the CLI fills them
// from the daemon when one answers.
func (a *App) Status(ctx context.Context) (control.StatusResult, error) {
	now := a.Clock.Now()
	result := control.StatusResult{QueueByLibrary: []control.QueueGroup{}, Running: []control.RunningSearch{}, Providers: []control.ProviderState{}, Instances: []control.InstanceState{}}
	counts, err := a.Repository.QueueCounts(ctx, now)
	if err != nil {
		return result, err
	}
	// The store orders the counts by instance and language, so each group's
	// counts are adjacent. A priority outside the three classes is not
	// counted, as before the split, and opens no group.
	for _, count := range counts {
		if queueClass(&control.QueueCounts{}, count.Priority) == nil {
			continue
		}
		last := len(result.QueueByLibrary) - 1
		if last < 0 || result.QueueByLibrary[last].Instance != count.Instance || result.QueueByLibrary[last].Language != count.Language {
			result.QueueByLibrary = append(result.QueueByLibrary, control.QueueGroup{Instance: count.Instance, Language: count.Language})
			last++
		}
		class := queueClass(&result.QueueByLibrary[last].QueueCounts, count.Priority)
		*class = class.Add(control.QueueClass{Total: count.Total, Due: count.Due, NextAt: count.NextAt})
	}
	for _, group := range result.QueueByLibrary {
		result.Queue.New = result.Queue.New.Add(group.New)
		result.Queue.Missing = result.Queue.Missing.Add(group.Missing)
		result.Queue.Upgrade = result.Queue.Upgrade.Add(group.Upgrade)
	}
	running, err := a.Repository.RunningSearches(ctx, now)
	if err != nil {
		return result, err
	}
	for _, item := range running {
		result.Running = append(result.Running, control.RunningSearch{MediaMatch: item.MediaMatch, Language: item.Language, LeaseUntil: item.LeaseUntil})
	}
	for _, id := range sortedProviderIDs(a.Config.Providers) {
		health, err := a.providerHealth(ctx, id, now)
		if err != nil {
			return result, err
		}
		if health.State != providerAvailable {
			result.Providers = append(result.Providers, control.ProviderState{ID: id, State: health.State, Until: health.Until, LastFailure: health.Failure})
		}
	}
	timings, err := a.Repository.ReconcileTimings(ctx)
	if err != nil {
		return result, err
	}
	waiting, err := a.Repository.WebhookInboxCounts(ctx)
	if err != nil {
		return result, err
	}
	for _, instance := range a.Config.Instances {
		state := control.InstanceState{Name: instance.Name, Webhook: "unmanaged"}
		timing := timings[instance.Name]
		state.LastReconcileAt, state.ReconcileFailures = timing.LastAttempt, timing.Failures
		scope, _, err := a.Repository.LibraryDiscoveryState(ctx, instance.Name)
		if err != nil {
			return result, err
		}
		state.DiscoveryComplete = scope != ""
		if a.Config.Server.WebhookBaseURL != "" {
			connection, err := a.Repository.WebhookConnection(ctx, instance.Name)
			if err != nil {
				return result, err
			}
			state.Webhook = "pending"
			if connection.ID > 0 {
				state.Webhook, state.WebhookCheckedAt = "connected", connection.CheckedAt
			}
		}
		result.Instances = append(result.Instances, state)
		result.Backlog.WebhookInbox += waiting[instance.Name]
	}
	result.Backlog.NotificationOutbox, err = a.Repository.NotificationBacklog(ctx)
	return result, err
}

// queueClass is the class of counts that holds searches of priority, nil for
// a priority outside new, missing and upgrade.
func queueClass(counts *control.QueueCounts, priority store.SearchPriority) *control.QueueClass {
	switch priority {
	case store.SearchPriorityImport:
		return &counts.New
	case store.SearchPriorityMissing:
		return &counts.Missing
	case store.SearchPriorityUpgrade:
		return &counts.Upgrade
	}
	return nil
}

// ListProviders shows every configured provider: its routes, its state now, and
// what its searches did over the last 24 hours. Read-only.
func (a *App) ListProviders(ctx context.Context) (control.ProvidersResult, error) {
	now := a.Clock.Now()
	counts, err := a.Repository.ProviderHistoryCounts(ctx, now.Add(-providerHistoryWindow))
	if err != nil {
		return control.ProvidersResult{}, err
	}
	last, err := a.Repository.LastInstalledByProvider(ctx)
	if err != nil {
		return control.ProvidersResult{}, err
	}
	routes := map[string][]control.ProviderRoute{}
	for _, language := range sortedLanguages(a.Config) {
		languageConfig := a.Config.Languages[language]
		for _, id := range languageConfig.Providers {
			routes[id] = append(routes[id], control.ProviderRoute{Language: language.String(), Tier: "preferred"})
		}
		for _, id := range languageConfig.FallbackProviders {
			routes[id] = append(routes[id], control.ProviderRoute{Language: language.String(), Tier: "fallback"})
		}
	}
	result := control.ProvidersResult{Providers: []control.ProviderRow{}}
	for _, id := range sortedProviderIDs(a.Config.Providers) {
		health, err := a.providerHealth(ctx, id, now)
		if err != nil {
			return control.ProvidersResult{}, err
		}
		count := counts[id]
		row := control.ProviderRow{
			ID: id, Type: a.Config.Providers[id].Type, Routes: routes[id],
			State: health.State, Until: health.Until, LastFailure: health.Failure, LastSuccessAt: last[id],
			Last24h: control.ProviderCounts{Installed: count.Installed, NoResult: count.NoResult, Rejected: count.Rejected},
		}
		if row.Routes == nil {
			row.Routes = []control.ProviderRoute{}
		}
		result.Providers = append(result.Providers, row)
	}
	return result, nil
}

func sortedProviderIDs[V any](providers map[string]V) []string {
	ids := make([]string, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

const (
	providerAvailable      = "available"
	providerCoolingDown    = "cooling_down"
	providerAuthFailed     = "auth_failed"
	providerQuotaExhausted = "quota_exhausted"
)

// providerHealthState is a provider's usability at one moment. Failure is the
// stored reason of the state that decided it.
type providerHealthState struct {
	State   string
	Until   time.Time
	Failure string
}

// providerHealth reduces a provider's saved states (one per scope) to one.
// A disabled state is auth_failed: only a credential rejection disables a
// provider, and it lasts until retry --provider or new credentials, so Until
// is zero. Otherwise only exhausted states whose reset lies in the future
// count, as in the provider gate:
// quota_exhausted for the download_quota and search_quota reasons, the only
// quotas the provider layer records (a 429 keeps the header limit with
// nothing remaining, so limit and remaining cannot tell a quota from a rate
// limit); cooling_down for any other reset (rate limit headers, service busy,
// a blocked client, a transient failure). The worst state wins in that order,
// and Until is the latest reset among the states of that kind.
func (a *App) providerHealth(ctx context.Context, providerID string, now time.Time) (providerHealthState, error) {
	states, err := a.Repository.ListProviderStates(ctx, providerID)
	if err != nil {
		return providerHealthState{}, err
	}
	best := providerHealthState{State: providerAvailable}
	rank := map[string]int{providerAvailable: 0, providerCoolingDown: 1, providerQuotaExhausted: 2, providerAuthFailed: 3}
	for _, state := range states {
		candidate := providerHealthState{Failure: state.Reason}
		switch {
		case state.Disabled:
			candidate.State = providerAuthFailed
		case state.Remaining > 0 || !state.ResetAt.After(now):
			// The provider gate blocks only an exhausted window that has not
			// reset; a window with requests left is a usable provider.
			continue
		case state.Reason == "download_quota" || state.Reason == "search_quota":
			candidate.State, candidate.Until = providerQuotaExhausted, state.ResetAt.UTC()
		default:
			candidate.State, candidate.Until = providerCoolingDown, state.ResetAt.UTC()
		}
		if rank[candidate.State] > rank[best.State] || (candidate.State == best.State && candidate.Until.After(best.Until)) {
			best = candidate
		}
	}
	return best, nil
}
