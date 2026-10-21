# Rejections and outcomes

Search outcomes, candidate rejection reason codes, how a rejection is identified and re-opened, and the owner's rejections from `reject`.

- [Search outcomes](#search-outcomes)
- [Reason codes](#reason-codes)
- [Rejection identity](#rejection-identity)
- [Rules versions](#rules-versions)
- [No expiry](#no-expiry)
- [User rejections](#user-rejections)
- [Clearing rejections](#clearing-rejections)

## Search outcomes

A finished search stores its outcome in `search_states.last_outcome` and in `search_history`.

| Outcome | Meaning |
| --- | --- |
| `installed` | A subtitle was installed (`workflow.OutcomeInstalled`) |
| `satisfied` | Nothing to do: an embedded track, a protected sidecar, or a retained managed subtitle that no candidate beats |
| `no_result` | No eligible candidate |
| `rejected` | Candidates existed but every one was rejected |
| `throttled` | Providers were cooling down; retried at the reset (see [Scheduling](scheduling.md#throttled-searches)) |
| `removed` | A reject's replacement run removed the managed subtitle; the underlying outcome is in `RemovedAfter`, reported as `reason` |
| `workflow_error` | A technical failure, on the failure schedule |
| `deleted` | The media was deleted |
| `unsupported_multi_episode` | The file's episodes cannot be served (see [Multi-episode files](../reconciliation.md#multi-episode-files)) |

Searches that keep a managed subtitle after their last allowed upgrade check end `satisfied` with state `complete`.

## Reason codes

Deterministic candidate failures are retained in `candidate_rejections` (`rejectionReasonCode` in `internal/workflow/service.go`). Technical failures never are.

| Code | Cause | Versioned |
| --- | --- | --- |
| `lapse_unsure`, `lapse_nothing` | A LAPSE verdict other than `solid`, after the media was re-checked readable | No |
| `lapse_invalid_output` | LAPSE's written subtitle failed size, encoding, syntax or timestamp validation (invalid timestamps even when LAPSE reported solid), or failed installation validation, such as ending more than five minutes past the media duration. Only the direct typed content-validation failure qualifies | No |
| `lapse_partial_coverage` | LAPSE's output for a range file ends before 75% of the runtime at installation, although the source passed the check before LAPSE | No |
| `lapse_repeated_empty_exit` | The second identical silent exit (see [LAPSE](lapse.md#repeated-empty-exit)) | No |
| `invalid_subtitle` | The content rules rejected the payload: invalid or oversized, unreadable, overlapping, negative timings (see [Normalization](matching.md#subtitle-normalization)), or a source installed without LAPSE failed installation validation | `/subtitle-rules-vN` |
| `pack_selection` | No unique member for the episode, ambiguous versions, disc markers, `versions_exhausted`; a release-numbered candidate or release-name member under a sync policy that would skip LAPSE (rules `release_numbered`, `release_episode_token`) | `/pack-selection-vN` |
| `language_mismatch` | The [language check](matching.md#language-check) found another language | No |
| `partial_coverage` | A range subtitle ends before 75% of the runtime, before LAPSE or as a source installed without LAPSE | No |
| `user_out_of_sync`, `user_wrong_language`, `user_wrong_release`, `user_poor_quality`, `user_other` | The owner's `reject --reason` ([below](#user-rejections)) | Never invalidated by tools or rules |

`candidate.skipped` (info) logs `reason_code=retained_rejection` (or the stored code) when an existing rejection removes a candidate; `candidate.rejected` (info) logs a new one.

Not rejections: provider refusals and error pages, transient and quota failures, process, protocol, filesystem and cancellation failures, missing artifact checksums, and composite errors containing a process exit. These stay retryable technical failures.

## Rejection identity

A rejection is scoped to one media, language and provider result. It applies only while all of these match:

- the media fingerprint: path, Arr file ID, size and mtime;
- the candidate signature: stable release metadata, Sonarr season, episode, absolute episode and episode title, range fields for range targets, provider-specific evidence such as the OpenSubtitles `download_version`, and `/member-<checksum>` for a pack member;
- the selected artifact checksum when known;
- for `lapse_*` and `pack_selection` rejections, the tool signature: LAPSE compatibility version and synchronization policy. `invalid_subtitle`, `language_mismatch` and `partial_coverage` come from subsyncd's own checks and match under any tool signature (`toolIndependentRejectionReasons`); any other reason is bound to it. When installation validation fails on LAPSE's output, the failure is recorded as `lapse_invalid_output` or `lapse_partial_coverage` instead (`syncedOutputFailure`), because LAPSE's offset or ratio caused it. A source installed without LAPSE (exact hash, score bypass, `sync.policy: never`) keeps the plain code, so a later policy change does not reopen it. Changing `langcheck` or `minimumRangeCoverage` reopens nothing either; `search --retry-rejected` does.

Volatile rating, popularity, download counts and temporary download URLs are not part of it. Set-like release names and direct-member evidence are sorted and deduplicated before hashing, without mutating provider results, so reordering or duplicates never cause a redownload.

Rejections are candidate-local. They never throttle a provider; a rejected cached pack member is skipped by checksum and the pack stays available to other episodes. Rejected candidates are removed before the three-result shortlist, so later-ranked results advance on the next job.

## Rules versions

`invalid_subtitle` and `pack_selection` follow subsyncd's own rules, so their candidate signature carries the rules version:

- `invalid_subtitle`: `/subtitle-rules-vN` from `pack.SubtitleRulesVersion`;
- `pack_selection`: `/pack-selection-vN` from `pack.SelectionRulesVersion`;
- a `versions_exhausted` rejection (stored as `pack_selection`) can follow from versions rejected as `invalid_subtitle`, so it carries both: `/subtitle-rules-vN/pack-selection-vM`.

`SubtitleRulesVersion` is 2 and `SelectionRulesVersion` is 3 (raised for the [release-name fallback](matching.md#release-name-numbers-release_episode_token)). A rejection with either reason under another version, including one recorded without a version (version 1), does not count. After changing a content or selection rule, bump the matching constant (in `internal/pack/manifest.go`) to re-open exactly the rejections the old rules made. Other reasons keep unversioned signatures.

## No expiry

Neither kind of rejection expires with time. The `expires_at_ns` column is always written as 0 and ignored on read; `explain` shows `expires=never`. There are no background download rechecks: a provider file silently replaced under unchanged evidence stays skipped until an explicit retry or another identity change.

## User rejections

`reject` stores the owner's rejection in the same table with reason `user_<reason>` (`store.UserRejectionPrefix`). The flow is in [Installation safety](../installation-safety.md#reject-replacement-and-removal).

- Identity: the installation's `candidate_signature` (including any `/member-<checksum>` scope) and `artifact_checksum` (migration 023), bound to the media fingerprint the subtitle was installed for, under the tool signature current at the time.
- An installation without a recorded signature stores `candidate_signature='*'` (`store.AnyCandidateSignature`), which matches that provider result under any signature for that file.
- `GetCandidateRejection` matches user rejections under any tool signature and artifact, so every lookup path (cached packs, exact candidates, broad shortlist, pack members) skips them.
- `RejectionActive` keeps them across tool and rule signatures and drops them only when the media fingerprint changes.
- A system rejection that later lands on the same key never overwrites a user reason.

## Clearing rejections

| Action | Clears |
| --- | --- |
| `search --retry-rejected` (`ClearCandidateRejections`) | subsyncd's own rejections and LAPSE strikes for the selected media and language, then searches. `user_%` rows are kept (matched with an escaped `LIKE`, since `_` is a wildcard). Any candidate that fails deterministically again is re-rejected |
| `reject --undo` (`ClearUserRejections`) | Only the owner's rejections for the file and language, and `replace_installed`; runs no search |
| A changed media fingerprint | Every rejection for the old fingerprint stops matching |
| Bumping a rules version | `invalid_subtitle` and `pack_selection` rejections of the old version |
| Changing `sync.policy` or the LAPSE version | System rejections LAPSE or the policy decided (`lapse_*`, `pack_selection`) under the old tool signature |
