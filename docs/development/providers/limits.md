# Provider limits

Rate limits, cooldowns, quotas, circuits, disabled credentials, download refusals, the route gate and cooldown resume progress. When searches are retried after a limit is in [Scheduling](scheduling.md#throttled-searches).

- [State and scopes](#state-and-scopes)
- [Pacing and concurrency](#pacing-and-concurrency)
- [Reset headers](#reset-headers)
- [Preflight and the final gate](#preflight-and-the-final-gate)
- [Transient-failure circuit](#transient-failure-circuit)
- [Disabled authentication](#disabled-authentication)
- [Provider-specific limits](#provider-specific-limits)
- [Download refusals](#download-refusals)
- [Fallback resets](#fallback-resets)
- [Route gate](#route-gate)
- [Resume progress](#resume-progress)
- [How status reports providers](#how-status-reports-providers)

## State and scopes

`provider_states` holds one row per provider instance and scope: `all`, `auth`, `search`, `download` and OpenSubtitles' `download_transfer`. A row has a reason (`rate_limit`, `download_quota`, `service_busy`, `client_blocked`, a transient-failure reason, or disabled authentication), remaining capacity and a reset.

- The state survives restart and is shared by every queued media item, so a provider outage produces one request, not one per file.
- Every persisted reset, whatever its source (headers, JSON bodies, fallbacks), is clamped to at most 48 hours ahead, and workflow cooldown and quota errors carry the clamped value.
- The most restrictive future reset wins. A response that was in flight when a longer cooldown began cannot shorten or clear it.
- A cooldown is never saved already over: `Client.PersistCooldown` replaces a reset that is not in the future with the provider's [fallback](#fallback-resets) for that kind, or 5 minutes (`defaultCooldown`) when there is none.
- State-read and state-write failures are typed and terminal through adapters and fallback handling (`provider.AvailabilityError`); they never permit a fallback.
- `subsyncd retry --provider NAME` clears every scope of one configured provider and makes due at once every search of a configured language whose last outcome was `throttled`. Throttled searches do not record which provider held them back, so one still blocked elsewhere is throttled again. Searches of deleted media, of languages no longer configured, and searches still leased are left alone. It changes no rejections or other schedules.

## Pacing and concurrency

- Each provider instance has its own token bucket (`requests_per_second`, `burst`) and active-request semaphore (`max_concurrent`).
- Accounts sharing an origin also share `provider_http.shared_origin_max_concurrent`; quota and authentication state stay per account.
- Both permits are held until the response body reaches EOF or is closed early; transport failures without a body release them at once. Adapters must consume or close every body.
- Availability is checked again after waiting for permits, so a queued request respects a cooldown that began while it waited.
- Availability reads consume no permits.

## Reset headers

These are read and persisted:

- `RateLimit` (draft-08 policy lists and the draft-07 `limit=…, remaining=…, reset=…` dictionary), `RateLimit-Policy`, and separate `RateLimit-Limit`/`-Remaining`/`-Reset`;
- `X-RateLimit-*`, where a reset too small to be a Unix epoch is read as delta seconds and an RFC 3339 timestamp is accepted;
- numeric or date `Retry-After`, honored only on non-success responses (some authentication endpoints send it on 2xx without a throttle);
- provider JSON reset values.

On a successful response, an exhausted window that resets within five seconds (per-second gateway windows) is not persisted; the token bucket paces those. HTTP 429 always exhausts the selected window, even when headers claim remaining quota.

## Preflight and the final gate

- Acquisition checks persisted download availability before the exact and broad phases and before reading the search cache, then again before each candidate download.
- A separate search preflight runs after a usable cache lookup and before fresh adapter calls. Expired, malformed and refresh-required cache entries cannot bypass it.
- Provider-wide (`all`), `download` and `download_transfer` cooldowns, and disabled authentication, block fresh acquisition. Search quotas only block searches.
- The pack cache runs first and can install while remote downloads are blocked.
- If a quota or circuit opens during a shortlist, later candidates from that provider are skipped before the adapter is called; other providers and the fallback tier stay eligible.
- A cooldown found at the final transport gate (including one that began after preflight) is marked locally suppressed; its completion log is debug, not a warning. Real remote failures keep their warn or error level.
- Download unavailability is kept in provider-error summaries, so a successful fallback install still remembers the preferred provider's reset.

## Transient-failure circuit

Network failures and HTTP 5xx open a persisted circuit for that provider instance and operation (`auth`, `search`, `download`, `download_transfer`).

- Consecutive failures retry after 1, 5, 15, then 60 minutes (capped at 60). A provider `Retry-After` takes precedence. `provider.circuit_opened` (warn) and `provider.recovered` (info) report it.
- A successful response clears the streak only after its body is consumed. Connection failures while reading a body open the circuit too.
- Closing early, local writer errors, malformed content, caller cancellation, and a redirect subsyncd itself refuses do not open the circuit.
- The transport keeps the failure count on a 401, 403 or 429, so a lasting block backs off 1, 5, 15, then 60 minutes instead of retrying every minute.
- A search circuit does not block downloads. The circuit never erases quota, rate-limit or disabled state. Transient and quota failures never reject candidates.

## Disabled authentication

A definitive credential rejection disables the configured instance until an operator fixes the credentials and runs `subsyncd retry --provider NAME`. There is no automatic expiry, and late responses cannot clear it. `provider.auth_disabled` (warn) reports it.

| Provider | Disables on |
| --- | --- |
| OpenSubtitles, Titlovi | A login HTTP 401 |
| SubDL | HTTP 401 from its own API or download host (the hosts of `base_url` and `download_base_url`) whose message names the key as rejected, only restates the status (`Unauthorized`, `Not authorized`), or is empty or not JSON; HTTP 403 whose message names the API key, or a token other than a download token, with `invalid`, `not valid`, `wrong`, `incorrect`, `missing`, `required`, `expired`, `revoked`, `disabled`, `banned`, `blocked`, `suspended`, `unauthorized`, `not authorized` or `unknown` (not `Download token expired` or `Subtitle not found for this key`) |
| SubSource | HTTP 401 |

Not a rejection:

- a Titlovi download 401 fails only that candidate;
- a SubDL 401 from another allowed host a download was redirected to (a CDN) fails only that candidate;
- any other SubDL 401 or 403 (empty body, HTML block page, bare `Forbidden`) opens the transient circuit (`transient_http_401`, `transient_http_403`);
- a SubSource 403 stays an ordinary failure, because it can come from a gateway challenge.

## Provider-specific limits

### OpenSubtitles

- HTTP 406 is the documented quota refusal.
- A 403 or 429 whose JSON names the quota is the same refusal: a `reset_time_utc`, `remaining` of 0, or a message that names downloads or the quota and says it is used up (`reached`, `exceeded`, `renewed`, `allowed N subtitles`/`allowed downloads`, `downloaded your allowed`). A bare `allowed` (`not allowed` for a blocked key) or a plain `Throttle limit reached` is not. Any other 403 fails that candidate; any other 429 stays a rate limit.
- Quota responses use one future reset for both persistence and the workflow error; a missing, malformed or past reset uses the 6-hour fallback.
- A successful link response that reports `remaining` 0 still downloads that subtitle and then persists the download-quota cooldown until `reset_time_utc`. If that cooldown cannot be saved, `provider.cooldown_save_failed` (warn) is logged and the link is still used.

### SubDL

SubDL limit messages are read in a 200 `status: false` answer and in any 4xx JSON body, on a 401 and a 403 alike (`limitCode` in `internal/provider/subdl/client.go`):

| Message | Kind | Pause |
| --- | --- | --- |
| `daily_limit`, `api_download_limit_exceeded`; or wording that names a limit or quota with `reached`, `exceeded` or `exhausted` and also names the day, downloads or a quota (`You have reached your daily download limit`, `Download limit reached`, `API quota exceeded`, `Quota exhausted`) | `search_quota` when a search received it, `download_quota` when a download did | Until SubDL's daily reset, 00:15 UTC |
| `service_busy` | `service_busy` | 1 hour |
| `rate_limit`; `too many requests` or `rate limit` wording; a reached limit that names none of the above (`Request limit reached`, `You have reached your limit`) | `rate_limit` | 15 minutes |

A server-supplied reset wins over the fallback. Messages such as `must not exceed the limit of 30`, `exceeds the character limit`, `unlimited downloads` or `Max file size exceeded for download` are not limits.

### SubSource

SubSource reports only its 60-request minute window, as `X-RateLimit-*` headers with an RFC 3339 reset. Its hourly (1,800) and daily (7,200) caps are not reported.

- A 429 reporting an exhausted minute window waits for the header reset.
- A 429 that does not (spare capacity in the headers, or no headers) means one of the unreported caps, since local pacing stays well under the minute limit. The adapter persists a key-wide (`all` scope) `download_quota` cooldown for one hour.
- `Retry-After`, when present, keeps the transport's normal handling.

### Gestdown

A 429 persists an operation cooldown, honoring rate-limit headers or `Retry-After` when they lie ahead, otherwise 5 minutes. HTTP 423 is a technical failure, never a cooldown (see [Gestdown](adapters.md#gestdown)).

## Download refusals

A provider can refuse a download with a page or message instead of a status code. The workflow checks a finished download before extracting it.

### What counts as a provider response

A download fails the candidate with `workflow.ProviderResponseError`, a technical failure rather than an `invalid_subtitle` rejection, when it:

- is not an archive and starts as an HTML page (or any `<` under a `text/html` content type);
- parses as a JSON object or array;
- is an archive whose only members are Titlovi's `titlovi_titl_plugin_message.srt`.

### What counts as a refusal

| Response | Refusal |
| --- | --- |
| The Titlovi placeholder (its text says the client is an unsupported plugin) | A client block (`client_blocked`) |
| A whole HTML document (starting `<!doctype html` or `<html`) whose visible text names a download limit, quota, too many requests or downloads, or an unauthorized download or access (`Unauthorized access`, within two words) | Yes |
| A whole HTML document with under 2 KB of visible text that asks to log in or has a password `<input>` | Yes (a login page) |
| JSON whose `error`, `message`, `status` or `detail` string values (also nested under those fields, and in each object of a top-level array) name one of these | Yes |
| An HTML fragment, or any `<` under `text/html` that is not a whole document | No: a provider error, since it could be a subtitle in another format |

Visible text excludes `script`, `style`, `noscript`, `textarea` and `template` elements, comments and tags. Field names never count (`remaining_quota` is not a quota message). Phrases match whole words (`quotation` is not `quota`), and underscores count as spaces (`limit_exceeded`). A bare `unauthorized` (`Unauthorized reproduction prohibited`) does not count.

### Confirmation

- An unconfirmed refusal fails only its candidate, like an ordinary error page: one deleted subtitle redirecting to a site page must not pause the provider.
- A refusal from a second, distinct candidate (`ResultID`) of the same provider within 30 minutes confirms it, from the same search or any other. `workflow.RefusalLedger` is shared by every run and language, holds the latest unconfirmed refusal per provider, and prunes entries older than 30 minutes.
- An explicit limit or quota in a JSON body is unambiguous and pauses at once. The Titlovi placeholder needs a second candidate like a page does.

### The pause

The workflow calls the adapter's `RecordDownloadRefusal` with the kind the message calls for (`ProviderResponseError.Cooldown`). It persists a `download`-scope cooldown and returns a `CooldownError`:

| Kind | When | Pause |
| --- | --- | --- |
| `rate_limit` | The message says `too many requests` or names a rate limit, and names no download limit, daily limit, quota or too many downloads | `FallbackReset(rate_limit)` for the provider, else 15 minutes |
| `client_blocked` | The Titlovi placeholder | 1 hour |
| `download_quota` | Anything else | `FallbackReset(download_quota)`, else 1 hour |

- When the confirmed refusal failed a candidate of the same run, that error unwraps to the cooldown too.
- OpenSubtitles API refusals are status codes handled by its client, so a refusal read from a download there comes from the file transfer. It still pauses the `download` scope with the 6-hour quota window, because every transfer starts with a link the API issued.
- Later candidates of that provider are skipped by the download preflight. A search whose failures are all unavailability ends as `throttled`, keeping its queue position and failure count.
- Refusals and other provider messages are never recorded as candidate rejections.

## Fallback resets

When a provider gives no usable reset, these compiled values apply. They are code constants (`FallbackReset` and `defaultCooldown` in `internal/provider/transport.go`, the refusal windows in `internal/provider/availability.go`), not settings.

| Provider | Condition | Pause |
| --- | --- | --- |
| Titlovi | Rate limit (429) | 5 minutes |
| Titlovi | Download quota | 1 hour (refusal window) |
| OpenSubtitles | Rate limit (429) | 1 minute |
| OpenSubtitles | Download quota (406, quota JSON, refusal) | 6 hours |
| SubDL | Rate limit | 15 minutes |
| SubDL | Daily download limit | Next 00:15 UTC |
| SubDL | Daily search limit | Next 00:15 UTC |
| SubDL | Service busy | 1 hour |
| SubSource | Rate limit | 1 minute |
| SubSource | Download quota (unreported hourly or daily cap) | 1 hour |
| Gestdown | 429 without a future reset (adapter) | 5 minutes |
| Any | Download refused as a client block | 1 hour |
| Any | Download refused as a rate limit, no provider fallback | 15 minutes |
| Any | Download refused as a quota, no provider fallback | 1 hour |
| Any | Other cooldown with no future reset and no fallback (`defaultCooldown`) | 5 minutes |
| Any | Transient-failure circuit | 1, 5, 15, then 60 minutes |
| Any | Disabled authentication | Until `retry --provider` |

## Route gate

Before each daemon lease, the worker's route gate (`internal/worker/route_gate.go`) checks every configured language for episodes and movies with `workflow.Service.RouteAvailability`: the same read-only download preflight, over the preferred then fallback providers that support the media kind.

- If every one reports a cooldown, quota or disabled state, that (language, kind) route is excluded from `LeaseDueSearchesExcept`. Its reset is the earliest cooldown or quota reset; with only disabled providers there is none, until `retry --provider`.
- Search-only scopes never pause a route, a route without a capable provider never pauses, and other routes keep leasing.
- When only single-episode-only providers can still download, only the route's multi-episode files pause (`RangesPaused`, `episode_ranges_only=true`).
- Pausing is re-evaluated on every dispatch, at least every poll, so a route resumes within one poll after its reset.
- Transitions log `queue.route_paused` (warn, `reset_at` or `reason=disabled`) and `queue.route_resumed` (info), again when the reset changes. A state-read failure leases nothing for that dispatch and logs `queue.route_check_failed` (error).
- Manual `search` runs the workflow directly and is never paused. While a route is paused, local-only satisfactions (embedded, sidecar, pack cache) on it wait too.

## Resume progress

For a first-install search (no installation for the current media file), a provider skipped because of a known search or download cooldown keeps the work unfinished even when other providers searched successfully. That holds when they found nothing, when every candidate they returned was set aside before download (below the minimum score, excluded by policy, or already rejected), and when another provider failed technically. The search is retried after the earliest reset among the skipped providers without advancing the missing or failure counters (see [Scheduling](scheduling.md#throttled-searches)). When every provider failed, the search is a technical failure instead.

Candidates downloaded and rejected in the run itself (by LAPSE, validation or a no-speech verdict) end the search `rejected` even while a provider is skipped. Not every such rejection is retained, so waiting for the reset could download and check the same candidates again at each reset; a retained rejection sets the candidate aside on the next search, which then waits for the skipped provider as above. A `reject` replacement follows the same rule and keeps the rejected subtitle while it waits.

### What is saved

During that unfinished cycle the search row holds a configured-order list of clean-empty provider IDs (`resume_providers_json`, migration 012) and an opaque route signature (`resume_route_signature`).

- A provider is clean-empty only after its broad search completed without candidates and, when applicable, its exact search did too. Candidate-bearing, disabled, throttled, canceled and failed providers stay pending.
- On the retry only the clean-empty providers are skipped; unfinished providers and any required fallback tier still run in order.
- The list is dated by its oldest answer (`resume_saved_at_ns`, migration 020). It holds as long as the missing schedule would wait after the search (`Scheduler.ResumeTTL`): 6 hours for a new import, 12 hours and then 48 hours as attempts grow, 7 days for media released long ago, then 14 days, and never less than the 6-hour search cache. The worker drops an older list before the search runs, and the completion then starts a fresh date. A quota wait of about a day therefore does not ask providers that found nothing again. A list without a date, or older than the 14-day safety cap (`store.ResumeProvidersMaxAge`), is dropped when the search is leased.
- A list that fails validation when leased (malformed JSON, duplicate or unknown IDs) does not fail the lease batch: that search runs the full route, its progress is reset, and `search.resume_reset` (warn, `component=store`) is logged.

### Binding and clearing

- The signature hashes the canonical language and ordered-tier route with the media path, file ID, size and nanosecond mtime. A changed route invalidates it, so a new or moved provider always runs.
- The worker supplies the saved state only when the signature matches the assembled workflow. The workflow validates it before inventory and again after adopting the live fingerprint.
- Live inventory replacement clears progress for every language and rechecks progress already copied into a running search; a path-only change may repeat providers once.
- Owner-guarded completion keeps the state only for throttled or retryable technical first-install outcomes. Imports, replacements, renames, terminal outcomes, upgrades, reruns and manual searches clear it, so a manual `search` always runs the complete route.
- The repository derives JSON-size and membership bounds from each accepted route, so every accepted route fits and provider IDs keep their exact identity, surrounding whitespace included.
- `job.completed` reports the count SQLite actually retained as `resume_provider_count`. The list and signature are never logged.

## How status reports providers

`status` and `providers` reduce a provider's saved states to one (`providerHealth`, `internal/app/status.go`):

| State | Meaning |
| --- | --- |
| `auth_failed` | Disabled authentication; lasts until `retry --provider`, so it has no `until` |
| `quota_exhausted` | An exhausted state with reason `download_quota` and a future reset: the provider's download quota |
| `cooling_down` | Any other exhausted state with a future reset: rate limits (including daily 429 request limits), service busy, a client block, a transient circuit |
| `available` | Nothing above |

The worst state wins in that order, and `until` is the latest reset among states of that kind.
