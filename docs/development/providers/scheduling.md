# Scheduling

When a search runs again. This page is the single source of truth for the missing, failure, throttle and upgrade schedules; other pages link here instead of repeating the numbers. Queue order among due searches is in [Queue and control](../queue-and-control.md#queue-order).

- [Overview](#overview)
- [Missing subtitles](#missing-subtitles)
- [Technical failures](#technical-failures)
- [Throttled searches](#throttled-searches)
- [Installed subtitles](#installed-subtitles)
- [Fallback installs](#fallback-installs)
- [The attempt counter](#the-attempt-counter)
- [Other schedules](#other-schedules)

## Overview

| Situation | Next search | Stops? |
| --- | --- | --- |
| Subtitle missing | 6 h, 12 h, 24 h, 3 d, 7 d, 14 d after import, then every 14 d | Never |
| Missing, release older than `aged_release_days` | 7 d, then every 14 d | Never |
| Technical failure | 1 min, 5 min, 15 min, 1 h, 6 h, 24 h, then daily | Never |
| Providers cooling down | At the earliest reset, plus up to 10% | Never |
| Preferred install | 7, 30 or 90 d by score, then backing off to 14, 30, 60, 90 d | After `upgrade_checks` unchanged checks |
| Fallback install of a recent release | 6 h, 12 h, 24 h, 3 d after install, then 7 d, then 14, 30, 60, 90 d | Never |
| Other fallback install | 7 d, then 14, 30, 60, 90 d | Never |
| Preferred exact hash | None | Immediately (until the media changes) |

Every due search refreshes local sidecars first, so each check also notices a subtitle another tool added. Provider search results are cached for six hours. No missing search or ordinary upgrade check is scheduled sooner than that after the previous search, so each of them asks the providers afresh. Technical-failure retries, throttled searches and cooldown-recovery checks can come sooner.

## Missing subtitles

`schedule.Scheduler.Missing` (`internal/schedule/scheduler.go`) applies to `no_result` and `rejected` outcomes, including an upgrade check that finds its sidecar gone.

- The milestones are measured from import or a schedule reset: immediately, then about 6 hours, 12 hours, 24 hours, 3 days, 7 days, 14 days, and every 14 days after that. The gaps between them (`missingIntervals` in `internal/schedule/backoff.go`) are 6 h, 6 h, 12 h, 48 h, 96 h, 168 h, then 336 h.
- Each gap gets fresh random jitter of 0 to +20%. Jitter only delays, so no retry lands inside the six-hour search cache of the previous search.
- A new import or replacement resets the schedule and is due at once.

### Aged releases

Media released at least `aged_release_days` (default 30, range 0–3650) before the completion skips the early retries:

- `Scheduler.Missing` uses `schedule.AgedMissingAttempt` (the 7-day step) for the delay and sets `SearchCompletion.MissingAttemptFloor`, so the stored attempt jumps there and the next no-result advances to the 14-day step.
- `aged_release_days: 0` disables the shortcut. An unknown release date (0) or one in the future keeps the full schedule.
- `media.released_at_ns` (migration 019) holds Sonarr's `airDateUtc` for the file's first episode, or Radarr's `digitalRelease`, else `physicalRelease`, else `inCinemas` + 90 days. A later update without a date keeps a known one. Discovery and every Arr re-read keep it current (see [Library discovery](../reconciliation.md#library-discovery)).
- `job.completed` adds `release_age_days` when the date is known and `schedule: "aged"` when the rule applied.

Cooldown resumes and technical failures do not advance the missing counter and are unaffected.

## Technical failures

`Scheduler.Failure` uses `schedule.WorkflowFailureDelay`: 1 minute, 5 minutes, 15 minutes, 1 hour, 6 hours, then every 24 hours. The failure counter is separate from the missing counter, which it does not change. A new import or replacement resets it.

Media-unavailable failures use the same delays but keep the search's queue position (see [Unreadable media](../queue-and-control.md#unreadable-media-and-the-media-breaker)). A job past its time limit completes as a technical failure (`workflow_error`).

## Throttled searches

A search that could not ask its providers because of cooldowns completes as `throttled`, and so does a first install that asked some providers but skipped one for a cooldown with a known reset and found nothing it could download (see [Resume progress](limits.md#resume-progress)). It is never stored as complete.

- The worker never sleeps through a cooldown. It releases the lease and schedules the search at or after the reset, plus fresh random positive jitter of up to 10% of the remaining cooldown.
- A reset less than a minute away, already past, or `Retry-After: 0` waits one minute (`minimumThrottleDelay`), so a provider is never asked in a loop.
- When two tiers are throttled, the earliest non-zero reset wins, so a disabled tier (which has no reset) cannot hide the other tier's reset.
- With no known reset at all (only disabled providers), the search waits 6 hours (`unknownThrottleDelay`) plus the jitter. The [route gate](limits.md#route-gate) keeps such a route paused until `retry --provider`.
- A retry after a quota reset skips providers that already found nothing within the missing schedule's next interval ([What is saved](limits.md#what-is-saved)); only older answers are asked again.
- A throttled completion keeps `queue_order_ns`, so jitter delays eligibility without changing queue position. It advances neither the missing nor the failure counter, and keeps the job's class.
- A replaced file's sidecar removal past the hourly cap also completes as `throttled` (reason `removal_deferred`; see [Removal cap](../installation-safety.md#removal-cap)).

Which providers are skipped on the retry is described under [Resume progress](limits.md#resume-progress).

## Installed subtitles

A managed non-exact subtitle is rechecked for a better candidate. `scheduleUpgrade` (`internal/workflow/upgrade.go`) decides when.

### Preferred-tier installs

- **First check** by release score: 7 days below 60, 30 days for 60–84, 90 days for 85 and above. Any score below 60 gets 7 days, including scores under 35 when `minimum_release_score` is lowered.
- **Unchanged checks** back off: each one moves the interval up through 14, 30, 60 and 90 days (only steps longer than the current one count), capped at 90 days.
- **Jitter**: ±10% on every ordinary check, the first included.
- **Stop**: once the unchanged-check count (`request.UpgradeAttempt`, the lease's stored attempt + 1) reaches `upgrade_checks` (`Service.MaxUpgradeChecks`, default 4, range 0–100, 0 = unlimited), the worker completes the search as `satisfied` with no next attempt; its state becomes `complete`.
- **Exact hash**: a preferred exact-hash install is terminal until the media fingerprint changes.

A clean unsuccessful check that keeps a valid managed subtitle completes as `satisfied` and stays upgrade work, including when nothing returns or every candidate fails eligibility. Successful scheduled installs and promotions restart the progression. Technical and throttled results keep their own backoff and do not advance the unchanged-check count. Media events and manual searches re-queue a completed search.

## Fallback installs

A subtitle from the fallback tier, exact hashes and score 100 included, is rechecked for a preferred-tier promotion. Fallback checks never stop: `upgrade_checks` does not apply.

### Recent releases

A fallback install of media released less than `aged_release_days` ago (`Service.AgedAfter`; release date known and not in the future, 0 disables) is checked at the same points as a missing search, because a preferred provider often publishes days after release:

1. about 6 hours after install;
2. about 12 hours after install;
3. about 24 hours after install;
4. about 3 days after install.

The first `recentFallbackChecks` (4) checks reuse the missing-search delays: check `attempt` (0 at install, then the unchanged-check count) waits `schedule.MissingDelay(attempt+1, …)`, which is 6 h, 6 h, 12 h and 48 h with the same delay-only jitter of 0 to +20%, so no check lands inside the previous search's cache.

After the fourth check, or once the release is older than `aged_release_days`, the ordinary fallback cadence follows: 7 days, then backing off to 14, 30, 60 and 90 days. When the installation was recorded while the release was recent (`installations.installed_at_ns`), the attempt is reduced by the four early steps (`max(0, attempt-4)`), so a release that ages out mid-way continues weekly instead of jumping ahead. Installs made after the release aged keep the unreduced count.

### Other fallback installs

The first check is after 7 days, then unchanged checks back off to 14, 30, 60 and 90 days with ±10% jitter.

### During a preferred cooldown

After a fallback install while a preferred provider was cooling down (including a partial outage), the next check is the earlier of the preferred provider's future reset and the scheduled check (weekly, or the first early step). A reset-recovery check is neither jittered nor delayed by the backoff.

There is no dedicated log event for these checks: they appear as `upgrade.scheduled` (info, `next_upgrade_at`) and, at debug, `candidate.fallback`.

## The attempt counter

`search_states.attempt` counts missing retries for import and missing work, and unchanged checks for upgrade work.

- Owner-guarded completion advances it for retained scheduled upgrades, resets it on installation or terminal satisfaction, and restarts it at one when an upgrade check finds the sidecar missing and completes its first unsuccessful acquisition.
- Media events (imports, renames, replacements) reset both counters and make the search due at once; during a live lease they request one rerun instead.
- A manual install restarts the schedule as a worker install does. Other manual results keep earlier queued work and can only bring a due time forward (see [Manual search lease](../queue-and-control.md#manual-search-lease)).
- `installations.fallback` (migration 006) records installation-time tier provenance. Current configured tier membership decides promotion.
- Configuration changes do not reopen terminal exact searches; a manual search can.

## Other schedules

| Schedule | Values | Described in |
| --- | --- | --- |
| Webhook inbox retries | 1 min, 5 min, 15 min, 1 h, 6 h, 24 h, then abandoned | [Webhook inbox](../reconciliation.md#webhook-inbox) |
| History reconciliation | Every 6 h; failures 5 min, 15 min, 1 h, then 6 h; deferred pages 1 h | [Timing](../reconciliation.md#timing) |
| Managed webhook connection | Every 6 h; failures 15 min, 1 h, then 6 h | [Schedule](../reconciliation.md#schedule) |
| Notification delivery | 1 min, 5 min, 15 min, then hourly; Arr rescans first due after 60 s | [Delivery and retries](../notifications.md#delivery-and-retries) |
| Provider transient circuit | 1, 5, 15, then 60 min | [Limits](limits.md#transient-failure-circuit) |
| Media breaker pause | 1 min doubling to 30 min | [The breaker](../queue-and-control.md#the-breaker) |
| Leases | 5 min, renewed every minute | [Leases](../queue-and-control.md#leases) |
