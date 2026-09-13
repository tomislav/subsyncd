# Provider-Specific Cooldown Resume

**Status:** Approved in chat; awaiting written-spec review

## Purpose

When an initial subtitle search is retried because one or more providers were unavailable, do not repeatedly contact providers that already completed the same search cycle without results. Persist enough provider-level progress to survive daemon restarts, then resume only the unfinished providers after their cooldown reset.

This refines the reset scheduling introduced by commit `58b511c`. It applies to first-install backfill only. Routine upgrades, ordinary complete-provider no-result searches, candidate selection, scoring, LAPSE, installation, and provider quotas remain unchanged.

## Selected approach

Persist a search-cycle resume record on `search_states`. The record contains:

- the configured route signature for the language;
- provider IDs that completed both applicable search phases with no candidates during the throttled cycle.

The worker passes a valid resume record into the workflow. Each tier coordinator excludes those provider IDs from exact and broad calls. Providers absent from the record run normally. This is durable across process restarts and avoids changing the general six-hour provider-result cache policy.

Alternatives rejected:

- Extending empty-result cache TTLs beyond a day is simple but changes freshness globally and can alternate between cache hits and repeated remote searches over a long cooldown.
- Keeping progress in memory avoids a migration but loses the state on deployment or crash, precisely when provider cooldown state is expected to survive.
- Persisting all provider candidates as completed would risk suppressing candidate-local or technical recovery. Only providers with a clean, candidate-free completion are skipped.

## Persistent state

Migration `012_provider_resume.sql` adds two non-null columns to `search_states`:

```text
resume_providers_json  TEXT NOT NULL DEFAULT '[]'
resume_route_signature TEXT NOT NULL DEFAULT ''
```

`resume_providers_json` is a JSON array of unique provider IDs in deterministic configured order. It is bounded by configured route membership and decoded strictly; malformed persisted state is a repository error rather than silently skipping providers. The signature is derived from the canonical language plus ordered preferred and fallback provider IDs with unambiguous separators. It contains identifiers only, never credentials, endpoints, query data, media paths, or candidate references.

Search leases carry both values. Search completion may replace or clear them under the existing lease-owner compare-and-swap. Attempt counters, priority, next-attempt time, lease ownership, rerun coalescing, and candidate rejection rows retain their current semantics.

Migration 012 also makes active, uninstalled, missing-priority rows whose current outcome is `throttled` immediately eligible once. This lets rows already processed by `58b511c` capture provider progress on the first run after deployment. Existing provider caches may avoid the repeated calls; if an entry has expired, that provider can be contacted one final time before its clean no-result state becomes durable. The migration changes neither cache expiry nor provider cooldown state.

## Provider phase accounting

`provider.SearchResult` reports, in addition to candidates and errors, the providers that completed the requested phase with zero candidates. A cache hit containing an empty candidate list counts as a clean empty completion. A skipped cooldown, disabled provider, persistence failure, cancellation, network error, or any returned candidate does not.

The workflow accumulates phase evidence per tier:

1. Exact phase runs for applicable exact-hash providers.
2. Broad phase runs after exact exhaustion under the existing policy.
3. A provider becomes resumable only if broad completed empty and, when it participated in exact, exact also completed empty.
4. A provider that returned any candidate in either phase is not marked resumable, even if that candidate was deterministically rejected.
5. A provider with any availability or technical error in either phase is not marked resumable.

The preferred and fallback tier results are merged in configured route order. Existing fallback rules still run the fallback tier before a throttled first-install result is returned. A fallback installation ends the current initial-search cycle and clears resume state; its existing preferred-reset promotion schedule remains unchanged.

## Resume lifecycle

For first-install work:

- If at least one provider remains unavailable and no provider installs a subtitle, return `throttled` with the earliest applicable reset and persist the union of prior valid resume providers and newly clean-empty providers.
- On the reset retry, skip the persisted providers and run only unfinished configured providers.
- If an unfinished provider remains unavailable, retain the record and schedule its next reset without advancing missing or failure attempts.
- When all unfinished providers run, the final workflow outcome follows existing rules (`installed`, `satisfied`, `no_result`, `rejected`, or technical failure) and clears resume state.
- A technical failure never marks its provider complete. Existing technical-failure scheduling applies and the clean-empty progress may be retained so recovery does not re-contact unrelated providers.

Resume state is ignored and cleared for routine upgrade work. It is also cleared by a media import/replacement/rename that resets the search, an explicit media/language retry, a successful installation or satisfied result, deletion/unsupported completion, and any route-signature mismatch. Manual searches always run the full configured route and clear stale resume state when they persist their result.

Configuration changes are safe: a changed preferred/fallback membership or order changes the route signature, invalidating the entire resume record. This ensures a newly added provider or a provider moved between tiers is not accidentally skipped.

Same-key catalog events during an active lease retain the existing lease and request one immediate rerun. Their authoritative reset clears stored resume state. Completion of the stale in-flight attempt must not restore its old resume record because the `rerun_requested` branch continues to ignore the stale completion schedule and metadata.

## Coordinator and workflow interfaces

The workflow request gains a bounded provider-exclusion set plus its validated route signature. Each per-language workflow exposes or is assembled with the matching signature. The coordinator filters exclusions only after normal language/media-kind applicability checks and before cache or availability access, so a skipped provider performs no cache read, provider-state read, log lifecycle, or remote request.

The workflow result returns the next resume-provider list. Provider IDs remain low-cardinality configuration identifiers. Structured completion logs add only `resume_provider_count`; they do not log the list, media path, cache key, or provider response.

If all providers in a tier are excluded, that tier returns an ordinary empty pass without fabricating a provider outage. The tier merger then relies on the unfinished provider's real outcome. Applicable-provider counts used for all-provider technical/throttle classification exclude resumed providers, so one resumed provider's technical failure cannot be masked by two deliberately skipped providers.

## Repository and worker behavior

`LeaseDueSearches` reads the resume columns into `SearchLease`. Before workflow execution, the worker validates the stored route signature against the current language workflow:

- match: pass the decoded provider set;
- mismatch or upgrade priority: run the full route and arrange to clear the stale state on completion;
- malformed JSON, duplicates, empty IDs, or IDs outside the bounded configured route: fail safely as a repository/validation error and do not contact providers.

`CompleteSearch` updates scheduling and resume state atomically under the lease owner. Throttled and retryable technical completions may retain the workflow-provided resume state. All terminal/successful/ordinary missing outcomes clear it. The `rerun_requested=1` branch clears it regardless of the stale completion payload.

The migration and repository never infer provider completion from candidate rows, audit logs, or expired cache contents.

## Safety and operational behavior

No provider quota is bypassed. Availability is still rechecked before every permitted search/download. Resume state suppresses only providers already proven clean and empty in the same unfinished cycle.

The stopped production container should remain stopped until a reviewed image containing migration 012 is available. Deployment applies the migration under the daemon's existing advisory lock. Restarting an older binary after migration 012 is unsupported by the current-schema check, consistent with the repository's forward-only embedded migration lineage.

No production database manipulation, manual provider search, cache rewrite, or media access is required. The migration is additive and preserves durable leases; abandoned leases become eligible under the normal lease-expiry rule.

## Testing and acceptance

Focused TDD must prove:

- coordinator exclusions cause no cache, availability, log-lifecycle, or remote access for skipped providers;
- exact-plus-broad clean-empty accounting marks only providers that completed every applicable phase without candidates/errors;
- two throttled providers out of three preserve the third provider as completed, then retry only the two unfinished providers;
- a still-throttled provider remains pending at its next reset while already-completed providers remain skipped;
- a resumed provider no-result clears resume state and returns to normal missing backoff;
- technical and candidate-bearing providers are never marked clean-empty;
- preferred/fallback merging, fallback installation, and promotion scheduling retain existing behavior;
- route changes, upgrades, imports/replacements/renames, manual retry, deletion, unsupported media, success, and same-key reruns clear or invalidate state as specified;
- migration 012 preserves attempts, failure attempts, priority, leases, installations, rejections, provider state, and cache TTLs while immediately rescheduling only eligible pre-feature throttled rows;
- restart round trips provider resume state through SQLite and skips completed providers;
- malformed persisted state fails closed without provider calls;
- structured logs expose only the count.

Run affected packages with `-race`, then the complete race-enabled suite, `go vet ./...`, tagged end-to-end tests, gofmt, and `git diff --check`. Tests use sanitized fixtures and local fake servers only; they must not contact production Arr instances or subtitle providers.

## Non-goals

- Changing the normal provider cache TTL or making it sliding.
- Persisting candidate download references or provider response bodies.
- Skipping providers that returned candidates, deterministic rejections, or technical failures.
- Changing missing/failure schedules after a complete provider cycle.
- Accelerating routine upgrades.
- Adding provider-specific queue rows, parallel workflows for one media/language, management HTTP routes, or operator-facing configuration.
