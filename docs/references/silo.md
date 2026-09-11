# Silo native scan API reference

Checked: 2026-09-05

Primary references:

- Silo Scan API documentation: <https://github.com/Silo-Server/silo-server/blob/main/docs/scan-api.md>
- Silo Autoscan documentation: <https://siloserver.org/docs/integrations/autoscan/>
- Silo API v2 program: <https://github.com/Silo-Server/silo-server/issues/135>
- Silo 1.0 cutover issue: <https://github.com/Silo-Server/silo-server/issues/886>
- Silo v2 migration-ledger PR: <https://github.com/Silo-Server/silo-server/pull/902>
- Silo source repository: <https://github.com/Silo-Server/silo-server>

Silo is pre-1.0, so this protocol boundary must be rechecked before a release or whenever the native scan API changes. Silo's accepted API-v2 program plans one bridge release exposing both API generations, followed by a 1.0 release that returns `410 Gone` for the entire `/api/v1` namespace.

As checked on 2026-09-04, the phase-1 migration ledger marks `POST /api/v1/scan` for porting, but its v2 method, path, and operation ID are still unset. Do not guess the future endpoint, rewrite a request after a `410`, or add an automatic fallback that could repeat a mutating request. Keep the current v1 adapter for current Silo releases. Once Silo publishes the v2 scan contract, add an explicit versioned adapter/configuration path with contract tests before upgrading a Silo deployment to 1.0.

## Contract used by subsyncd

Silo's native API accepts `POST /api/v1/scan` on its main API listener. The production container exposes that listener internally on port `8080`; deployments using Silo's published Compose may expose it differently on the host. The route requires an admin JWT or admin API key in the `Authorization: Bearer …` header. A valid targeted scan returns `202 Accepted`.

The request body is:

```json
{
  "path": "/mnt/media/movies/Movie"
}
```

`subsyncd` maps the media-file path into Silo's namespace and sends its parent directory rather than either individual file. Silo resolves that directory to a subtree scan, which discovers the newly written sibling subtitle. Optional rewrites select the longest matching local prefix at a path-component boundary before choosing the parent directory. `/` is valid on either side of a mapping: `/media -> /` removes the local mount prefix, while `/ -> /mnt/media` adds the Silo mount prefix. Relative endpoints, traversal, and escaped results fail closed before any request.

The endpoint is available when Silo runs in `integrated` or `api` mode. It is not registered in `proxy` or `transcode` mode. The path must exist from Silo's filesystem view, use a supported media extension, and belong to an enabled Silo library.

## Local decisions

Notification deduplication includes the committed media fingerprint and subtitle
destination, as well as notifier, media row, language and subtitle checksum.
Identical subtitle content installed for a replacement file must trigger a new
scan. Retries of the same installation remain deduplicated. Existing outbox rows
remain valid; changing the key does not automatically replay historical scans.

| Observed behavior | Decision | Proof |
|---|---|---|
| Silo documents native, authenticated targeted scans through `POST /api/v1/scan`. | Use the native route instead of the legacy Jellyfin-compatible Autoscan route. | `internal/notifier/silo_test.go` validates method, route, bearer header, body, and accepted response. |
| A media-file target can complete without walking the sibling subtitle, while a directory target performs a subtree scan. | Send the mapped media file's parent directory, never the individual media or subtitle file. | `TestSiloPostsNativeTargetedScanWithMappedParentDirectory` plus the 2026-09-05 production file-versus-subtree test. |
| The native API listener is port `8080` inside the current production Silo container. | Point container-network examples to `http://silo:8080`; do not assume the host-published port. | Live production reachability and accepted-scan test. |
| Silo and `subsyncd` may see different mount paths. | Retain deterministic, boundary-aware longest-prefix mapping, including `/` on either side, before selecting the mapped media parent directory. | `TestRewritePathSupportsRootMappingsAndLongestPrefix` and `TestSiloPostsNativeTargetedScanWithMappedParentDirectory`. |
| Admin API keys are credentials and Silo is still pre-release. | Never persist the key or include response bodies/transport URLs in errors; reject redirects and keep the notifier optional. | Failure, timeout, redirect, and secret-redaction notifier tests. |
| Notification transport can be unavailable after a subtitle was safely committed. | Persist a deduplicated notification job and retry it independently; never roll back acquisition. | Worker and repository notification lifecycle tests. |
| Silo plans to retire `/api/v1` at 1.0, while the v2 scan route is not yet assigned in the migration ledger. | Support the documented current route only; do not speculate or retry a mutating notification across API versions. Add v2 after its contract is published. | API-v2 epic #135, cutover issue #886, and migration-ledger PR #902. |

## Rejected compatibility path

Silo still exposes `POST /Library/Media/Updated` on its optional Jellyfin-compatible listener for legacy external Autoscan deployments. `subsyncd` does not need that compatibility layer because it can call the documented native scan API directly.
