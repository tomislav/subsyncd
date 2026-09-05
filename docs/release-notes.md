# Release verification

## Initial implementation baseline — 2026-09-04

The release candidate was verified as a native Linux arm64 image on an arm64 Docker host. The Dockerfile selects matching Linux amd64 or arm64 Go/LAPSE artifacts when BuildKit supplies `TARGETARCH`, and derives the native architecture for legacy Docker builds.

Resolved build and runtime components:

| Component | Resolved version |
| --- | --- |
| Go toolchain | 1.27.1 (`go 1.27.1`) |
| Arr API client | `github.com/cplieger/arrapi/v2` v2.0.5 |
| Runtime base | Debian 13.2 slim, glibc 2.41-12 |
| FFmpeg / FFprobe | 7.1.5-0+deb13u1 |
| LAPSE release assets | v2.0.5; the bundled executable reports `2.0.0` |
| SQLite driver | `modernc.org/sqlite` v1.58.0 |
| ZIP archive reader | Go 1.27 standard library `archive/zip` |
| RAR archive reader | `github.com/nwaples/rardecode/v2` v2.4.1 |

The checksummed LAPSE archives are:

- linux/amd64: `95f1eb35d83ee0084ba968f145c23c3c79aeb08c995b0f584b2177b016b67014`
- linux/arm64: `23226fea64f7141687b764e5d080b6ed4f9e2fbed476938363e993bd3705ee17`

The final local verification image was `sha256:2051a4f528c586dda1046436f3a0072612632fc8158ecc62dbbaa35ac5418384` (linux/arm64, 218,540,133 bytes, UID/GID `10001:10001`). This is a local content ID, not a published registry digest.

Verification passed:

- `go test ./... -race`
- `go vet ./...`
- `go test ./test/e2e -tags=e2e -v`
- credential-gated provider contract suite compilation and no-credential skip behavior
- standalone and root-stack Compose rendering, with and without the opt-in profile
- native legacy-Docker production build without explicit target arguments
- in-container `subsyncd --version`
- in-container `subsyncd doctor` using real SQLite, FFprobe, and LAPSE
- complete Go module graph inspection for unexpected runtime dependencies

The attempted linux/amd64 smoke build on this arm64 host reached the amd64 Go toolchain but its legacy Docker/QEMU environment crashed inside `go mod download`. The amd64 LAPSE archive itself was downloaded and checksum-verified. Run a native amd64 or BuildKit/buildx CI build before publishing the amd64 image.

## Stable Arr reconciliation dependency boundary — 2026-09-05

The pending release pins `github.com/cplieger/arrapi/v2` v2.0.5 and Go 1.27.1. Arrapi is private to the catalog package and owns bounded, retried history/current-entity requests; subsyncd's existing hardened detail client remains temporarily for metadata that scoring and provenance require.

Arrapi retry diagnostics pass through a subsyncd-owned `slog` handler that discards every upstream message, attribute, and group. It emits only `arr.request_retry` or `arr.request_retries_exhausted` with the configured instance. Returned errors retain bounded operation, status, kind, and retryability fields without response bodies, request paths, URLs, API keys, or media paths. Client construction remains offline and accepts reverse-proxy base paths.

Media rows now persist the stable Arr movie/episode identity separately from the replaceable movie-file/episode-file identity. Entity imports, renames, upgrades, known and unknown deletions, audit rows, schedules, and cursor advancement are atomic. Whole-second history requests overlap the fractional cursor and stable history event IDs make replay safe. Deliberately unmapped current entities are consumed outside scope, while unsafe path/filesystem failures retain the prior cursor.

Legacy zero-entity rows adopt identity only on later successful hydration; startup remains offline and does not backfill or scan the complete Arr library. The documented compatibility limit is a historical deletion that arrives before legacy adoption: Arr supplies no deleted physical file ID, so subsyncd records an unknown audit and leaves that row unchanged. Failed reconciliation retries independently per instance after 5 minutes, 15 minutes, 1 hour, then 6 hours; success restores the normal six-hour interval.

Fresh local verification passed the complete race-enabled Go suite, `go vet ./...`, the tagged race-enabled end-to-end suite, standalone Compose rendering, contract scans, and `git diff --check`. A native arm64 image built with Go 1.27.1 as `subsyncd:arrapi-local`; its local content ID is `sha256:9138534acd0cd9e4196abf7a95693656f50bf92aa7427551c427dbe98d66d4fb`, configured user `1000:1000`, and `--version` reports `subsyncd arrapi-local`. This is a local image, not a registry digest; nothing was published or deployed by this verification.

## Rootless identity defaults — 2026-09-04

The image now defaults to UID/GID `1000:1000`. Standalone and root-stack Compose definitions interpolate `PUID` and `PGID` from the host environment or project `.env` file into both `user:` and `/tmp` tmpfs ownership, defaulting each to `1000`. This is a rootless runtime override: the container does not interpret those names, start as root, create arbitrary users, or change mounted-path ownership.

Compose also passes `TZ`, defaulting to `Europe/Zagreb`, and the image explicitly includes Debian timezone data. Operators must ensure `/data` and mapped media roots are accessible to the selected numeric identity.

Both Compose files rendered successfully with defaults and with `PUID=1234 PGID=2345 TZ=UTC`. The native arm64 image built successfully and reported `sha256:27a06cd026c0445c69dfb91f50c512315e51f8dfbeba65a2627c62d3a4ea000a` (218,820,223 bytes, configured user `1000:1000`). A container runtime check confirmed UID/GID `1000:1000` and the Zagreb UTC offset. This is a local content ID, not a published registry digest.

## Priority dispatch and LAPSE tournament — 2026-09-04

The pending release adds SQLite-backed import/missing/upgrade priority, coalesced same-key reruns, capacity-aware webhook wake dispatch, and configurable `worker.max_concurrent` (default 1, range 1–8). The LAPSE workflow now evaluates the top-three cap lazily by release-score tier, analyzes equal-score ties before choosing by confidence, synchronizes one winner, and preserves fallback plus rejection semantics.

The pre-change Hades baselines were 12m11s for Arrival Croatian and 24m55s/six LAPSE passes/about 22.1 GB for 1917 Croatian. Publication verification and the immutable image tag will be recorded after the final repository gate and GitHub Actions complete. No production canary is run automatically.
