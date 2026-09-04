# Silo native scan API reference

Checked: 2026-09-04

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

Silo's native API accepts `POST /api/v1/scan` on its main API listener (normally port `8090`). The route requires an admin JWT or admin API key in the `Authorization: Bearer …` header. A valid targeted scan returns `202 Accepted`.

The request body is:

```json
{
  "path": "/mnt/media/movies/Movie/Movie.mkv"
}
```

`subsyncd` sends the media-file path rather than the subtitle sidecar path. Silo resolves a media-file path to a targeted file scan, and its scanner refreshes the associated external-subtitle inventory. Optional longest-prefix rewrites translate the path visible to `subsyncd` into the path visible inside Silo.

The endpoint is available when Silo runs in `integrated` or `api` mode. It is not registered in `proxy` or `transcode` mode. The path must exist from Silo's filesystem view, use a supported media extension, and belong to an enabled Silo library.

## Local decisions

| Observed behavior | Decision | Proof |
|---|---|---|
| Silo documents native, authenticated targeted scans through `POST /api/v1/scan`. | Use the native route instead of the legacy Jellyfin-compatible Autoscan route. | `internal/notifier/silo_test.go` validates method, route, bearer header, body, and accepted response. |
| A media-file target performs a file scan and refreshes external subtitle inventory. | Send the mapped media-file path, never the newly written subtitle path. | `TestSiloPostsNativeTargetedScanWithMappedMediaPath` and the tagged end-to-end install test. |
| The native API commonly uses port `8090`. | Point example configuration to `http://silo:8090`. | `config.example.yaml` is loaded by `TestExampleConfigurationLoads`. |
| Silo and `subsyncd` may see different mount paths. | Retain deterministic, boundary-aware longest-prefix mapping. | `TestSiloPostsNativeTargetedScanWithMappedMediaPath`. |
| Admin API keys are credentials and Silo is still pre-release. | Never persist the key or include response bodies/transport URLs in errors; reject redirects and keep the notifier optional. | Failure, timeout, redirect, and secret-redaction notifier tests. |
| Notification transport can be unavailable after a subtitle was safely committed. | Persist a deduplicated notification job and retry it independently; never roll back acquisition. | Worker and repository notification lifecycle tests. |
| Silo plans to retire `/api/v1` at 1.0, while the v2 scan route is not yet assigned in the migration ledger. | Support the documented current route only; do not speculate or retry a mutating notification across API versions. Add v2 after its contract is published. | API-v2 epic #135, cutover issue #886, and migration-ledger PR #902. |

## Rejected compatibility path

Silo still exposes `POST /Library/Media/Updated` on its optional Jellyfin-compatible listener for legacy external Autoscan deployments. `subsyncd` does not need that compatibility layer because it can call the documented native scan API directly.
