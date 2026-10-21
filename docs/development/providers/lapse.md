# LAPSE

LAPSE is the only synchronization engine. This page covers when it runs, how candidates are compared, its time limits and cache, and how its failures are classified. LAPSE v2.2.4 is the compatibility baseline; startup and `doctor` require it.

- [Synchronization policy](#synchronization-policy)
- [Invocation](#invocation)
- [Score-tier tournament](#score-tier-tournament)
- [Failures](#failures)
- [Repeated empty exit](#repeated-empty-exit)
- [Time limits](#time-limits)
- [Speech-profile cache](#speech-profile-cache)
- [Diagnostics](#diagnostics)

## Synchronization policy

Exact OpenSubtitles hash matches always bypass LAPSE. For every other candidate, `sync.policy` decides:

| Policy | Behavior |
| --- | --- |
| `confidence` (default) | A non-pack first install bypasses LAPSE when its release score is at least `sync.bypass_score` (default 75) and the anchors below are present. Everything else uses LAPSE. The stored verdict is `score_bypass` |
| `always` | LAPSE for every non-exact candidate |
| `never` | No LAPSE for any non-exact candidate, including packs, runtime packs, upgrades and edition-unknown candidates. The stored verdict is `sync_disabled` (mode `bypass`, reference `policy`) |

The `confidence` bypass needs, with the default switches (all `true`):

- `sync.require_identity_anchor`: an external-ID match or a normalized title and year;
- `sync.require_release_group`: a release-group match;
- `sync.require_episode_evidence`: explicit episode evidence for TV;
- an explicit edition match when Radarr identifies the movie's edition: the cut, or for an edition without a cut (such as IMAX) all of its picture variants. An edition with neither (Criterion) does not restrict the bypass; an unknown candidate edition is eligible but needs LAPSE; a known mismatch is rejected.

and, by default, also uses LAPSE for:

- `sync.lapse_for_packs`: pack members;
- `sync.lapse_for_upgrades`: replacing a managed subtitle.

Rating and popularity can add points but never replace an anchor. Relaxing the switches raises the chance of installing a wrong or unsynchronized subtitle.

With `never`, eligibility, identity rejection, the minimum score, upgrade deltas, archive selection and subtitle validation are unchanged, and the bypass score and switches are ignored. Because versions are ranked by LAPSE confidence, a pack supplying more than one version of the episode is rejected as `pack_selection` (rule `versions_ambiguous_without_sync`) instead of installing an arbitrary member.

The policy is part of the rejection tool signature, so changing it lets earlier timing and version-ambiguity rejections be reconsidered. So does a new LAPSE version. Rejections from subsyncd's own checks (`invalid_subtitle`, `language_mismatch`, `partial_coverage`) are kept. `candidate.selected` logs `selection_mode` (`exact_hash`, `score_bypass`, `sync_disabled` or `lapse`), and `explain` shows the stored verdict.

## Invocation

- Each required candidate is prepared once with a new explicit output: `--output`, `--no-backup`, `--json`, `--strict` and `--no-sidecar`, passed as an argument array. One invocation supplies both the decision metrics and the synchronized artifact.
- Only a `solid` result with a valid written output can install. Malformed protocol output is a technical failure. So is `unchanged: true` on the install run, because LAPSE then writes nothing. `unchanged` and `offsets` (LAPSE 2.3) are optional, so 2.2.4 reports still parse; when present, `offsets` has one entry per part.
- Cancellation kills the whole LAPSE process group. Raw stdout, stderr and command lines are never exposed or logged.
- Generated cues with valid intervals but decreasing start times are stably reordered (`subtitleorder.Reorder`) before final validation; equal-start cues keep their order. Negative or reversed intervals reject as `lapse_invalid_output`.
- `lapse.sync_started` (info) precedes the call; `lapse.sync_completed` (info for `solid`, warn otherwise) or `lapse.failed` follows with the verdict metrics (see [Logging](../logging.md#lapse)).

## Score-tier tournament

The best three eligible non-hash candidates are a cap, not an eager batch.

1. Split them by release score and work from the highest score down.
2. A unique top scorer is prepared alone; if it installs, lower tiers are never downloaded.
3. Candidates tied at one score are all prepared so LAPSE confidence can choose. A solid result with confidence of at least 0.9 and full agreement and coverage is accepted at once and the rest of its tier is skipped (`candidate.early_accepted`, info); sorting still installs the best prepared result.
4. The winner's retained output installs directly, without another LAPSE call. Other viable outputs stay available as installation fallbacks.
5. Only an exhausted tier opens the next lower score.

With distinct scores and a successful leader, one invocation is expected; three tied candidates need up to three. Exact hashes and score bypasses need none. Original source checksums identify rejections, and cached pack sources stay immutable.

## Failures

| Failure | Classification |
| --- | --- |
| `unsure` or `nothing` verdict | Candidate rejection (`lapse_unsure`, `lapse_nothing`), after the media is re-checked readable. Unreadable media ends the search as media-unavailable instead |
| Invalid generated subtitle content | `lapse_invalid_output` rejection |
| The second identical silent exit | `lapse_repeated_empty_exit` rejection ([below](#repeated-empty-exit)) |
| Direct no-speech result | A media-validation failure, not evidence against the subtitle |
| Timeouts, crashes, diagnostic-bearing or malformed output, other exit codes, cancellation, runner, protocol and filesystem failures, missing checksums | Technical failure on the failure schedule; the candidate is not quarantined |
| Composite errors containing a process exit | Technical as a whole, even when another cause is deterministic; no strike and no rejection |

In every failure case the job workspace is removed and an installed subtitle stays untouched.

### Timeout latch

The first `syncer.TimeoutError` in a workflow attempt is latched on the request (`lapseTimeout`, shared by member, pack-cache and fallback-tier requests of that attempt). The time goes into decoding the media, and LAPSE writes no speech profile for an unfinished run, so every later run for that file would hit the same limit.

- Later non-exact candidates reaching `SynchronizeCandidate` get the latched error without a run.
- Exact hashes and bypasses never read the media and proceed normally, in the fallback tier too.
- An attempt that installs nothing ends as exhausted acquisition on the failure schedule, reported as `lapse_timeout`.
- `TimeoutError` is returned only when the run's own limit expired, never when the caller's context ended first.

## Repeated empty exit

Migration 013 adds `candidate_lapse_failures`, a bounded strike for the only process failure that can become a rejection.

- A strike is recorded only when a selected artifact with a checksum gets a direct or unary `%w`-wrapped `ProcessExitError` with exit code 2 and completely empty stdout and stderr (`exit_2_empty_output`).
- The first observation stays a technical failure on the ordinary backoff.
- A second observation in a separate workflow attempt with the identical media fingerprint, candidate and member identity, artifact checksum, LAPSE compatibility and policy signature, and failure signature atomically writes the permanent rejection `lapse_repeated_empty_exit` and deletes the strike. The workflow then advances to the next version, candidate or provider without another invocation.
- One rejection and artifact identity is prepared and counted at most once per workflow attempt across pack cache, exact, broad, preferred and fallback acquisition, so a cache and provider duplicate cannot confirm its own strike.
- Changed media, candidate, member scope, artifact, LAPSE version or policy starts a fresh first strike.
- `search --retry-rejected` and every rejection reset clear strikes for the selected media and language; routine searches leave them alone. `explain` reports only confirmed rejections.

## Time limits

LAPSE makes no network request, but it reads the media to build a speech profile; on network storage the first run can read most of the file.

- Each run is limited to `sync.timeout` (default 30 minutes) or, when larger, the media size divided by `sync.min_read_rate` (default 4 MiB/s), measured when the run starts.
- That is capped at `sync.max_timeout` (default 4 hours, or `sync.timeout` if longer). A `sync.max_timeout` below `sync.timeout` is rejected at load.
- The comparison is made in seconds before conversion, so extreme sizes or rates cannot overflow. An unreadable size keeps `sync.timeout`.
- The whole search job has its own limit of twice the longest run (see [Job time limit](../queue-and-control.md#job-time-limit)).

## Speech-profile cache

Profiles live in `<data_dir>/lapse-cache`, so later candidates for unchanged media reuse them. The confidence bypass avoids the media read for strong first installs.

- `lapse_cache.ttl` (default `720h`, must be positive) is measured from file mtime (the last profile write), with no refresh on cache hits.
- Mutating assembly prunes expired entries once; the daemon checks hourly. The shared LAPSE runner lock excludes cleanup; an active invocation postpones the sweep to the next check.
- Cleanup uses rooted directory operations, validates the root's identity, rejects a symlink root, and removes only regular files named `<16 lowercase hex>.spans` or `.spans.tmp` at or past the cutoff. It does not traverse directories or follow entry symlinks.
- Cleanup is best effort: `lapse.cache_cleanup_failed` (warn) or `lapse.cache_cleaned` (debug, `removed_count`), never with paths.
- Read-only commands never prune profiles.

## Diagnostics

`subsyncd analyze-sync --media PATH --subtitle PATH` runs a read-only `--dry-run` on a private copy with a throwaway cache, so it always decodes the whole audio track, and never installs a sidecar.
