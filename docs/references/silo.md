# Silo native scan API reference

Checked: 2026-09-04

Primary references:

- Silo Scan API documentation: <https://github.com/Silo-Server/silo-server/blob/main/docs/scan-api.md>
- Silo Autoscan documentation: <https://siloserver.org/docs/integrations/autoscan/>
- Silo source repository: <https://github.com/Silo-Server/silo-server>

Silo is pre-1.0, so this protocol boundary must be rechecked before a release or whenever the native scan API changes.

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

## Rejected compatibility path

Silo still exposes `POST /Library/Media/Updated` on its optional Jellyfin-compatible listener for legacy external Autoscan deployments. `subsyncd` does not need that compatibility layer because it can call the documented native scan API directly.
