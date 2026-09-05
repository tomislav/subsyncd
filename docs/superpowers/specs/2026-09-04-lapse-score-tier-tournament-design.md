# LAPSE Score-Tier Tournament Design

> Current contract: [single-run LAPSE preparation](../specs/2026-09-05-single-run-lapse-design.md) supersedes the acquisition dry-run/second synchronization sequence described below. Score-tier ordering remains; acquisition emits one synchronization event pair per required candidate.


**Status:** Approved for implementation

## Purpose

Reduce repeated media reads while preserving score-based candidate choice, LAPSE validation, deterministic rejection, upgrade safety, and fallback behavior. The Example Movie A production canary evaluated three candidates with both dry-run and synchronization passes, requiring six reads of a 22.1 GB media file and 24 minutes 55 seconds. The normal path should require one analysis and one synchronization pass when the highest-scored candidate is unique and valid.

This change is independent of daemon queue scheduling. It applies identically to manual searches and daemon workflows.

## Alternatives considered

The selected approach is a score-tier tournament: analyze only the highest remaining metadata-score tier, use LAPSE confidence inside a tied tier, synchronize only the selected candidate, and stop before touching lower tiers once a winner installs.

Analyzing all three finalists and then synchronizing one would reduce six passes to four, but it still reads lower-scored candidates that cannot win under the score-first ordering. Trying only the single highest-scored result would be faster but would lose recovery when that subtitle is malformed, incomplete, unsynchronizable, or fails during final synchronization.

Arbitrary early-stop thresholds such as a ten-point score gap are unnecessary. Exact score tiers express the existing lexicographic policy without a new tuning parameter: a solid candidate with a higher metadata score always outranks a lower-scored candidate, while LAPSE confidence decides among equal scores.

## Preserved ranking semantics

Provider results continue through existing language, media-kind, identity, forced-only, hearing-impaired, minimum-score, installed-candidate, and upgrade-policy gates. Exact provider hash remains score 100 and bypasses LAPSE. Existing score-policy bypasses also remain unchanged.

Candidates remain initially ordered by:

1. Metadata score, descending.
2. Provider priority, ascending.
3. Provider rating, descending.
4. Provider popularity, descending.
5. Provider ID and result ID for stable ordering.

The shortlist remains capped at three eligible non-hash candidates. No new user-facing threshold or candidate-count option is introduced.

Candidates with the same metadata total form a score tier. Within a tier, successfully analyzed candidates are ordered by:

1. LAPSE analysis confidence, descending.
2. Provider priority, ascending.
3. Provider rating, descending.
4. Provider ID and result ID for stable ordering.

This is equivalent to the current final ordering wherever the same candidates validate, while allowing exact early stopping between unequal score tiers.

A score-bypass result retains confidence zero, matching current behavior. Therefore a genuinely LAPSE-analyzed candidate with the same metadata score and positive confidence ranks ahead of it; a unique higher-score bypass still stops the tournament immediately.

## Tournament flow

The workflow processes score tiers from highest to lowest. Candidate downloads and archive extraction are lazy: a lower tier is not downloaded merely because it entered the three-result shortlist.

For every candidate in the current tier:

1. Recheck persisted deterministic rejection state.
2. Download and select the correct archive member using existing pack rules.
3. Apply minimum-score and upgrade-policy gates before LAPSE.
4. If exact hash or score policy permits bypass, retain the source as a prepared result with its existing bypass provenance.
5. Otherwise run LAPSE dry-run analysis once.
6. Retain only a `solid` analysis and its confidence; record existing deterministic rejection evidence for a non-solid or media-validation failure.
7. Preserve transient provider and infrastructure errors for the existing throttled/error outcome rules rather than blacklisting them.

If the tier has no viable results, proceed to the next lower score tier. If it has one or more, rank that tier and attempt finalization in order:

- A bypass result needs no synchronization pass and can proceed directly to installation.
- A LAPSE-analyzed result is synchronized once to a candidate-specific output path. The stored synchronization provenance retains the synchronization mode, offset, ratio, agreement, coverage, parts, and splits, while its confidence remains the comparable dry-run confidence.
- If synchronization is `solid`, attempt the existing fingerprint-guarded atomic installation. Success ends the tournament immediately; no lower tier is downloaded or analyzed.
- If synchronization fails, try the next already-analyzed candidate in the same tier. Persist a candidate rejection only when existing rules classify the failure as deterministic; retain transient errors only for final retry/throttle classification.
- If every viable candidate in the tier fails synchronization, continue with the next lower tier.

Installation errors that indicate stale or changed media retain their current behavior and must not cause the workflow to install a lower candidate against obsolete media metadata.

## Early stopping

Early stopping is structural rather than heuristic:

- A unique highest-scored candidate that analyzes and synchronizes successfully ends after two LAPSE media passes.
- When several candidates tie for the highest score, analyze that entire tied tier so confidence can choose the winner, then synchronize only the best result.
- Lower-scored candidates are considered only when every candidate in each higher tier is unusable or final synchronization fails.
- Exact-hash and permitted score-bypass candidates preserve their current zero-LAPSE path.

For the recorded Example Movie A candidate scores `39`, `38`, `37`, and `37`, the new normal path would analyze and synchronize only score-39 candidate `304766`. The fourth result remains outside the three-candidate shortlist. This statement describes expected control flow; production performance must be measured after implementation rather than inferred from the baseline.

## Internal structure

Split the current combined `synchronize` helper into focused phases with private workflow types:

```text
downloadedCandidate  candidate + score + provider priority + selected source path
analyzedCandidate    downloaded candidate + analysis result + bypass flag
finalizedCandidate   candidate + score + synchronization provenance + installable path
```

The analysis phase must not create a synchronized output file. The finalization phase must not rerun dry-run analysis. Temporary files remain inside the existing per-workflow workspace and are removed by its established cleanup path.

Keep scoring and LAPSE process execution behind their existing package boundaries. The workflow orchestrates phases; it does not parse LAPSE JSON itself or move ranking rules into the syncer package.

## Rejections, retries, and upgrades

Artifact checksums used for deterministic rejections are calculated from the downloaded/selected subtitle, not from a synchronized derivative. Repeated deterministic failures continue to use the existing candidate signature and checksum guards.

A provider download failure, provider cooldown, timeout, context cancellation, or filesystem infrastructure error remains non-deterministic unless current rules already classify it otherwise. Such a failure must not blacklist a candidate merely to advance the tournament.

Installed-candidate reassessment remains metadata-only when provider/result ID and full media fingerprint are unchanged. The tournament is entered only when a genuine initial installation or permitted upgrade needs validation. Upgrade score delta, future upgrade scheduling, checksum idempotence, rollback, notification, and Silo behavior remain unchanged.

## Observability

Workflow decisions add explicit stages for:

```text
tournament_tier
lapse_analysis
early_stop
lapse_finalize
fallback
```

Each decision contains provider/result identity and a redacted reason such as score tier, non-solid analysis, synchronization failure, or lower tiers skipped. `explain` continues to show persisted provider candidates and the installed LAPSE provenance. It does not persist temporary analysis results after the workflow ends.

## Testing and acceptance

Tests must prove the exact call counts and selection behavior:

- a unique top-score solid candidate causes one analysis and one synchronization call;
- lower-score candidates are neither downloaded nor analyzed after that success;
- all candidates tied at the highest score are analyzed, confidence selects the winner, and only that winner is synchronized;
- a non-solid unique leader is rejected and the next score tier is evaluated;
- a synchronization failure falls back to the next analyzed candidate in the same tier without repeating analysis;
- exhausting a tied tier proceeds to the next lower tier;
- transient failures preserve retry/throttle behavior and are not persisted as deterministic rejections;
- exact hash and configured score bypasses make zero LAPSE calls;
- season-pack extraction, installed-candidate reassessment, upgrade policy, stale-media fingerprint protection, identical-checksum idempotence, and cleanup retain their existing behavior;
- deterministic ordering is stable when score, confidence, provider priority, and rating tie;
- context cancellation stops the current LAPSE process group and does not start another candidate;
- race-enabled tests remain clean.

Run the complete race-enabled Go suite, `go vet ./...`, tagged end-to-end tests, and `git diff --check`. Update architecture, operations, README feature behavior, and the implementation-status ledger. After publishing a verified image, repeat a narrowly scoped production canary only with explicit preservation/rollback steps because the current Example Movie A and Example Movie B sidecars are valid production artifacts.

## Non-goals

- Changing metadata score weights or hard-rejection rules.
- Combining metadata score and LAPSE confidence into a new numeric score.
- Parallel LAPSE execution for candidates from the same media file.
- Caching LAPSE analysis across workflows or media fingerprint changes.
- Adding FFsubsync or another synchronization engine.
- Making the shortlist size or early-stop behavior configurable.
- Removing fallback after a candidate fails analysis or synchronization.
