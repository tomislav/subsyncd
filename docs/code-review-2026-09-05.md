# Codebase review — 2026-09-05

Reviewed commit: `1ca056e22ac416d2fd25b302fb5fe27bdf759ecd`.

Scope: runtime assembly/configuration/CLI/HTTP, Arr catalogs, provider coordination and normalization, matching, inventory, pack selection, synchronization, installation, persistence, workers, scheduling, notifications, and existing verification. This is a correctness review, not an exhaustive security audit. No production behavior was changed. P1 means fix before broad deployment; P2 means a narrower functional defect that should be addressed.

## Findings

### 1. [P1] Arr history parsing silently drops real records

Locations: `internal/catalog/sonarr.go:125–136`, `internal/catalog/radarr.go:86–97`, `internal/catalog/reconcile.go:33–38`.

The history decoders expect top-level `episodeFileId` and `movieFileId`. The upstream resources instead expose episode/movie IDs and optional nested resources. Consequently real records decode with a zero file ID and are skipped. Reconciliation treats this as success and advances its cursor, so `scan` and recovery of missed webhooks discover no media through this path.

Resolve the current file through the episode/movie resource, deduplicate resolved file IDs, and test realistic history payloads. Existing catalog tests manufacture the file-ID fields expected by the implementation, masking the contract mismatch. Also define how a resolved episode/movie without a current file should be skipped without failing the entire history batch.

Contract evidence: [Sonarr HistoryResource](https://raw.githubusercontent.com/Sonarr/Sonarr/develop/src/Sonarr.Api.V3/History/HistoryResource.cs), [Radarr HistoryResource](https://raw.githubusercontent.com/Radarr/Radarr/develop/src/Radarr.Api.V3/History/HistoryResource.cs), checked on the review date. No live Arr instance was queried.

### 2. [P1] OpenSubtitles episode identity conflicts with Sonarr series identity

Locations: `internal/provider/opensubtitles/client.go:312–320,347–349`, `internal/catalog/sonarr.go:113`, `internal/match/score.go:23–28,138–139`.

Sonarr supplies the series IMDb ID. OpenSubtitles episode results supply an episode IMDb ID plus a separate `parent_imdb_id`; the adapter drops the parent and stores the episode ID as the comparable identity. The matcher rejects the mismatch before considering an exact hash. Thus a correct episode, including a hash-matched one, is rejected when both IDs are populated.

A [response reported in the OpenSubtitles repository](https://github.com/opensubtitles/vlsub-opensubtitles-com/issues/11) demonstrates the distinction: Doctor Who S03E07 carries episode ID `1000251`, parent ID `436992`, and a true hash-match flag. Normalize episode results to the same series-identity namespace as the catalog, retaining separate episode evidence. Cover both exact and broad results, missing parent metadata, and genuine identity conflicts.

### 3. [P1] The first unusable exact result hides all alternatives

Locations: `internal/provider/coordinator.go:47–50`, `internal/workflow/service.go` candidate evaluation and rejection filtering.

The coordinator returns only the first candidate marked `ExactHash`, before the workflow checks identity, hearing-impaired policy, or the persistent rejection ledger. If that first result is HI under the default policy, a later usable non-HI exact result is discarded and broad search never runs. A quarantined or identity-conflicting first exact result causes the same starvation. Cached searches repeat the failure.

Make the exact-search stopping decision depend on usable candidates. Preserve exact alternatives and permit continuation when policy or preparation rejects them; an exact result should become terminal only after successful selection/installation. Add an integration test across the real coordinator and workflow rather than testing each exclusively with a fake counterpart.

### 4. [P1] Resolution suffixes become season-pack episode ranges

Location: `internal/pack/select.go:19`, used by selection at lines 54–77 and cached member evidence.

`Show.S01E01.1080p.srt` is parsed as episodes 1 through 1080. With only that member, selecting episode 2 incorrectly returns episode 1. With ordinary E01 and E02 resolution-tagged members, both appear to contain episode 2 and the valid pack is rejected as ambiguous. That deterministic failure can quarantine the candidate for 30 days. LAPSE remains a subsequent guard; this finding does not assume every wrong selection is installed.

Require unambiguous, bounded range syntax and distinguish release metadata from episode endpoints. Preserve genuine ranges. Add release-style filenames containing resolutions and years to selector tests. Temporary overlay tests reproduced both wrong selection and false ambiguity.

### 5. [P1] Installation and durable notification are disconnected

Locations: `internal/worker/worker.go:177–180`, `internal/store/repository.go:1016–1039`, `internal/app/app.go:450–454`.

The worker enqueues notification only after installation has committed. A crash or enqueue error in between leaves a subtitle with no notification. Recovery of an exact installation returns `satisfied`, whose completion path does not recreate the missing notification. Ordinary manual `search` calls also bypass the worker enqueue path entirely, even when Silo is configured.

Persist a checksum-deduplicated notification intent in the installation transaction, shared by worker and manual searches, then deliver independently. Alternatively, provide an equally durable reconciliation mechanism for installations missing notification intents. A temporary worker overlay reproduced an enqueue failure followed by recovery with zero notifications queued.

### 6. [P1] Forced-only subtitles can be installed as complete exact matches

Locations: `internal/provider/opensubtitles/client.go:85–97,303–320`, `internal/workflow/service.go:487–488,519–520`.

OpenSubtitles queries do not exclude foreign-parts-only results, and response decoding drops `foreign_parts_only`. A forced-only result can therefore retain `ExactHash` without retaining its incomplete-subtitle classification. Independently, extraction recognizes a `Movie.forced.srt` filename, but the workflow's single-file shortcut bypasses the selector's forced-member filter. It can install that payload as `Movie.en.srt`, bypass LAPSE, and record terminal exact provenance. The user receives only foreign-dialogue captions and future searches stop.

Filter forced-only provider metadata before exact selection and enforce the extracted-member policy even for a single non-pack file. Two temporary overlay tests reproduced metadata loss and an installation of `Movie.forced.srt` as `Movie.en.srt` with zero LAPSE calls. The upstream response field is also visible in the [OpenSubtitles response example](https://github.com/opensubtitles/vlsub-opensubtitles-com/issues/11).

### 7. [P2] A deleted exact subtitle cannot be reacquired

Locations: `internal/workflow/service.go:166–173`, `internal/workflow/upgrade.go` exact-installation guard.

After inventory refresh finds no subtitle, the workflow still returns `satisfied` solely because an exact installation row matches the media fingerprint. Deleting the sidecar and running manual `search` therefore makes no provider request. Installation provenance is not proof that the subtitle still exists.

Apply terminal and upgrade provenance only to a present, checksum-matching managed sidecar. A missing file needs first-install recovery semantics, including at the later upgrade gate; continue protecting user-modified files. A temporary overlay reproduced `outcome=satisfied` and zero searches with an empty inventory.

### 8. [P2] Additional YAML documents are silently ignored

Location: `internal/config/config.go:189–207`.

`yaml.Unmarshal` reads the first document into a node, which is then marshaled back into a single-document stream. The subsequent `ensureSingleDocument` check therefore cannot see trailing documents from the original file. Appending `---` followed by `sync: {policy: always}` succeeds while the effective policy remains `confidence`, contradicting the documented strict configuration behavior.

Validate the original input stream for exactly one document before expansion/remarshal. A temporary test calling the real `Load` function reproduced this silent acceptance.

### 9. [P2] Silo mappings involving the filesystem root are ignored

Location: `internal/notifier/silo.go:115–119`.

Configuration accepts `/` as a mapping endpoint, but trimming its trailing slash turns it into an empty string and causes the mapping to be skipped. `/media → /` leaves `/media/movies/Movie.mkv` unchanged instead of producing `/movies/Movie.mkv`; `/ → /mnt` similarly does nothing. Silo receives an unintended path.

Preserve root semantics during prefix matching and suffix joining, or explicitly reject unsupported mappings during configuration. Temporary overlay tests reproduced both cases without HTTP calls.

### 10. [P2] Provider concurrency permits end before downloads finish

Location: `internal/provider/transport.go:33–34`.

The deferred gate release runs when `HTTP.Do` returns headers, before callers consume the response body. A second workflow can begin another download while the first body is still streaming, despite a per-provider or shared-origin concurrency limit of one. Local pacing does not fix this for slow response bodies.

Hold permits through body EOF/Close with an exactly-once release wrapper, and release immediately when no usable response exists. Temporary fake-RoundTripper tests reproduced the second request being admitted while the first body remained open for both independent limits, without network traffic.

## Verification and improvements

The existing full race suite, `go vet ./...`, and tagged `test/e2e` race suite passed. The initial sandboxed suite could not bind loopback ports; rerunning with permission for local fake servers passed. No live provider, Arr, or Silo requests were used. The e2e suite uses controlled substitutes and does not establish compatibility with every live upstream deployment or packaged LAPSE runtime.

Focused review tests were run through temporary Go overlays, leaving production code and committed tests unchanged. Their intentional failures demonstrate missing behavior; they are distinct from the passing existing suite. Temporary overlays are under `/tmp/subsyncd-review-root`, `/tmp/subsyncd-acquisition-review`, `/tmp/subsyncd-provider-review-overlay`, and `/tmp/subsyncd-review-*-overlay.json`.

The highest-value improvements are realistic sanitized upstream contract fixtures, tests that cross coordinator/workflow boundaries, and failure injection at installation/outbox/restart boundaries. Fix the P1 findings before broad deployment; then add deleted-sidecar recovery and configuration/path edge cases. Keep the existing safety boundaries, offline startup, bounded extraction, and independent retry schedules intact.
