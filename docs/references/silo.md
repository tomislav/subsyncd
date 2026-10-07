# Silo native scan API reference

Checked: 2026-09-14

Primary references:

- [Silo API v2 documentation](https://silo.cerberus.group/api/v2/docs)
- [Silo API v2 OpenAPI contract](https://silo.cerberus.group/api/v2/openapi.json)

## Contract used by subsyncd

`subsyncd` supports only `POST /api/v2/scan` (`startLibraryScan`), on Silo's native API listener. Configure the server base URL, for example `http://silo:8080`, without an API suffix. The adapter appends `/api/v2/scan`, preserving any reverse-proxy base path. There is no v1 adapter, version selection, or fallback.

The route requires admin authorization through `Authorization: Bearer …`. `subsyncd` uses an admin API key and sends `Content-Type: application/json`. The profile headers are optional and are omitted.

The request body is:

```json
{
  "path": "/mnt/media/movies/Movie"
}
```

`ScanStartInputBody` permits `path` and optional string `library_id`; neither is required by the schema. `subsyncd` always supplies a nonempty mapped parent directory and omits `library_id`, allowing Silo to resolve the library from the path. The path must be accessible to Silo within a configured library root.

A valid scan returns `202 Accepted` with `status: "accepted"`, `mode` (`library`, `subtree`, or `file`), and an opaque string `library_id`. This acknowledges dispatch, not scan completion. The notifier does not consume or persist response bodies.

`subsyncd` maps the media-file path into Silo's namespace and sends its parent directory so a subtree scan discovers the sibling subtitle. Optional rewrites select the longest matching local prefix at a path-component boundary before choosing the parent directory. `/` is valid on either side of a mapping: `/media -> /` removes the local mount prefix, while `/ -> /mnt/media` adds the Silo mount prefix. Relative endpoints and traversal fail closed before any request.

## Delivery and retries

Notification deduplication includes the committed media fingerprint and subtitle destination, as well as notifier, media row, language, and subtitle checksum. Identical subtitle content installed for a replacement file triggers a new scan. Retries of the same installation share one outbox intent while it is pending. Once an intent is delivered or terminally rejected it releases its key, so publishing the same subtitle again (for example after the sidecar was deleted and reinstalled) enqueues a new scan.

Silo marks this operation `x-silo-retry-safety: non_retryable`; it provides no scan idempotency key or replay guarantee. The existing outbox deliberately retains at-least-once delivery for subtitle refreshes: timeouts, transport failures, HTTP 408, 429, and 5xx retry asynchronously. An uncertain response or recovered lease can therefore dispatch a duplicate targeted scan. The client makes one request per delivery attempt, never follows redirects, and never retries against another API version. Other failures, including 404, 409, 410, and 422, are terminal. Errors expose only bounded categories and status codes, never credentials, response bodies, or transport URLs.

Installation and notification intent commit atomically before asynchronous delivery. A Silo failure never rolls back an installed subtitle. Existing pending outbox rows use v2 after upgrading; completed rows are not replayed. No configuration or database migration is needed, but the configured Silo server must support API v2.

## Contract tests

`internal/notifier/silo_test.go` verifies the v2 route (including a reverse-proxy prefix), method, bearer header, strict path-only request body, mapped parent directory, and accepted response. It also covers status classification with no version fallback, timeout behavior, redirect rejection, credential/body redaction, and boundary-aware path mapping. Tests use local fake servers and sanitized data.
