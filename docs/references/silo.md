# Silo notification compatibility reference

Checked: 2026-09-04

Primary references:

- Silo Autoscan documentation: <https://siloserver.org/docs/integrations/autoscan/>
- Silo source repository: <https://github.com/Silo-Server/silo-server>

Silo is pre-1.0, so this protocol boundary must be rechecked before a release or whenever Silo changes its Jellyfin compatibility layer.

## Contract used by subsyncd

Silo's documented legacy/external Autoscan compatibility surface accepts `POST /Library/Media/Updated` on its Jellyfin-compatible listener (normally port `8096`). Authentication uses the `X-Emby-Token` header. A successful update returns `204 No Content`.

The request body is:

```json
{
  "Updates": [
    {
      "path": "/mnt/media/movies/Movie/Movie.mkv",
      "updateType": "Modified"
    }
  ]
}
```

`subsyncd` sends the media-file path rather than the subtitle sidecar path because Silo documents media paths for targeted library resolution. Optional longest-prefix rewrites translate the path visible to `subsyncd` into the path visible inside Silo.

## Local decisions

| Observed behavior | Decision | Proof |
|---|---|---|
| Silo supports the Jellyfin-compatible media-update endpoint and `X-Emby-Token`. | Adopt only this narrow endpoint; do not build a general Silo client. | `internal/notifier/silo_test.go` validates method, route, header, body, and success. |
| The compatibility listener commonly uses port `8096`, distinct from Silo's web UI. | Tighten the example configuration and document the distinction. | `config.example.yaml` points to `http://silo:8096`. |
| Silo and `subsyncd` may see different mount paths. | Add deterministic, boundary-aware longest-prefix mapping. | `TestSiloPostsJellyfinCompatibleMediaUpdateWithMappedMediaPath`. |
| Admin API keys are credentials and Silo is still pre-release. | Never persist the key or include response bodies/transport URLs in errors; reject redirects and keep the notifier optional. | Failure, timeout, redirect, and secret-redaction notifier tests. |
| Notification transport can be unavailable after a subtitle was safely committed. | Persist a deduplicated notification job and retry it independently; never roll back acquisition. | Worker and repository notification lifecycle tests. |

Bazarr was not used for this adapter. The official Silo compatibility documentation is authoritative.
