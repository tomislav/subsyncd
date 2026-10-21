# Providers and matching

How subsyncd finds, scores, times and installs subtitles for one media file and language. For provider setup and the knobs an operator changes, see the [provider guide](../../providers.md).

| Page | Covers |
| --- | --- |
| [Adapters](adapters.md) | Language routing, the five built-in providers, and how to add one |
| [Matching](matching.md) | Search order, caches, identity gates, the score model, editions, sources, language checks, archives, packs and episode ranges |
| [Limits](limits.md) | Rate limits, cooldowns, quotas, circuits, download refusals, the route gate, resume progress and the fallback reset table |
| [Scheduling](scheduling.md) | When missing, failed and installed subtitles are searched again: the single source of truth for every retry schedule |
| [Rejections](rejections.md) | Reason codes, search outcomes, rejection identity and versioning, and user rejections |
| [LAPSE](lapse.md) | Synchronization policy, the score-tier tournament, timeouts, the LAPSE cache, the repeated-exit strike and upgrading LAPSE |

## Glossary

| Term | Meaning |
| --- | --- |
| Route | A language's ordered provider list: `providers` (preferred tier) then `fallback_providers` (fallback tier). A route is paused per language and media kind |
| Tier | One of the two provider groups of a route. The fallback tier runs only after the preferred tier's acquisition is exhausted |
| Phase | Exact (file-hash search) or broad (metadata search). Exact runs first within a tier |
| Candidate | One provider result, identified by `(provider_id, result_id)` |
| Shortlist | The best three eligible broad candidates of a tier; exact candidates are uncapped |
| Release score | The 0–100 score from identity and release signals ([Matching](matching.md#score-model)). `minimum_release_score` (default 35) gates eligibility |
| Score tier | Candidates with equal release score, prepared and compared together |
| Bypass | Installing without LAPSE: an exact hash, a policy-qualified strong first install (`score_bypass`), or `sync.policy: never` (`sync_disabled`) |
| Solid | The LAPSE verdict that allows an install. `unsure` and `nothing` reject the candidate |
| Pack | An archive with several subtitles: provider-declared season packs, or a runtime pack found after download |
| Member | One subtitle inside an archive; member-scoped rejections name its checksum |
| Rejection | A retained, non-expiring record that one candidate is unusable for one media fingerprint and language ([Rejections](rejections.md)) |
| Cooldown | A persisted provider pause for a scope (`all`, `auth`, `search`, `download`, `download_transfer`) until a reset ([Limits](limits.md)) |
| Throttled | A search outcome: providers were cooling down, so the search is unfinished and waits for their reset. That includes a first install where some providers answered but a provider was skipped and no candidate could be downloaded ([Resume progress](limits.md#resume-progress)). Never stored as complete |
| Provenance | The `installations` row that makes a sidecar managed: path, checksum, provider, candidate, score and sync verdict |
| Promotion | Replacing a fallback-tier installation with a preferred-tier one |
