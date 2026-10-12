# Silo native scan API reference

The part of Silo's API that subsyncd uses. The authoritative contract is your Silo server's own documentation at `/api/v2/docs` (OpenAPI at `/api/v2/openapi.json`) and the upstream Silo project. How intents are queued, deduplicated and retried is in [Notifications](../development/notifications.md).

## Contract used by subsyncd

subsyncd supports only `POST /api/v2/scan` (`startLibraryScan`) on Silo's native API listener. There is no v1 adapter, version selection or fallback.

- **Base URL**: configure the server base URL, for example `http://silo:8080`, without an API suffix. The adapter appends `/api/v2/scan` and keeps any reverse-proxy base path.
- **Authorization**: `Authorization: Bearer <api_key>` with an admin API key. The request sends `Content-Type: application/json`; the optional profile headers are omitted.
- **Body**:

  ```json
  {
    "path": "/mnt/media/movies/Movie"
  }
  ```

  `ScanStartInputBody` permits `path` and an optional string `library_id`. subsyncd always sends a non-empty mapped parent directory and omits `library_id`, so Silo resolves the library from the path. The path must be visible to Silo inside a configured library root.
- **Response**: a valid scan returns `202 Accepted` with `status: "accepted"`, `mode` (`library`, `subtree` or `file`) and an opaque `library_id`. This acknowledges dispatch, not completion. subsyncd treats any 2xx as delivered and does not read or store the body.

## Path mapping

subsyncd maps the media file's path into Silo's namespace and sends its parent directory, so a subtree scan discovers the sibling subtitle.

- `silo.path_mappings` rewrites the longest matching local prefix at a path-component boundary, before the parent directory is taken.
- `/` is valid on either side: `/media -> /` removes the local mount prefix, and `/ -> /mnt/media` adds Silo's.
- Relative endpoints and traversal fail closed before any request.

## Status handling

| Answer | Handling |
| --- | --- |
| Any 2xx | Delivered |
| Timeout, transport failure, 408, 429, 5xx | Retried with the outbox backoff |
| 409 Conflict | Retried: a scan of the same scope is already active and may already have passed the new subtitle |
| Any other status (404, 410, 422, …) | Terminal: the intent completes as a permanent error and logs `notification.failed` (error) |

- Each attempt makes one request with a 15-second timeout, never follows redirects, and never retries against another API version.
- Errors expose only bounded categories and status codes, never credentials, response bodies or URLs.
- Silo marks this operation `x-silo-retry-safety: non_retryable` and offers no idempotency key. The outbox deliberately keeps at-least-once delivery, so an uncertain response or a recovered lease can dispatch a duplicate targeted scan.
- Intents leased together whose mapped scan directory matches (two episodes of one season, two languages of one file) share one request, because Silo rejects a second scan of a scope that is still active.

## Contract tests

`internal/notifier/silo_test.go` covers the v2 route (including a reverse-proxy prefix), method, bearer header, the path-only body, the mapped parent directory, the accepted response, status classification without version fallback, timeouts, redirect refusal, credential and body redaction, and boundary-aware path mapping. Tests use local fake servers and sanitized data.
