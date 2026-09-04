# Release verification

## Initial implementation baseline — 2026-09-04

The release candidate was verified as a native Linux arm64 image on an arm64 Docker host. The Dockerfile selects matching Linux amd64 or arm64 Go/LAPSE artifacts when BuildKit supplies `TARGETARCH`, and derives the native architecture for legacy Docker builds.

Resolved build and runtime components:

| Component | Resolved version |
| --- | --- |
| Go toolchain | 1.27.0 (`go 1.27`) |
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
- complete Go module graph inspection with no Bazarr module/runtime dependency

The attempted linux/amd64 smoke build on this arm64 host reached the amd64 Go toolchain but its legacy Docker/QEMU environment crashed inside `go mod download`. The amd64 LAPSE archive itself was downloaded and checksum-verified. Run a native amd64 or BuildKit/buildx CI build before publishing the amd64 image.

## Rootless identity defaults — 2026-09-04

The image now defaults to UID/GID `1000:1000`. Standalone and root-stack Compose definitions interpolate `PUID` and `PGID` from the host environment or project `.env` file into both `user:` and `/tmp` tmpfs ownership, defaulting each to `1000`. This is a rootless runtime override: the container does not interpret those names, start as root, create arbitrary users, or change mounted-path ownership.

Compose also passes `TZ`, defaulting to `Europe/Zagreb`, and the image explicitly includes Debian timezone data. Operators must ensure `/data` and mapped media roots are accessible to the selected numeric identity.

Both Compose files rendered successfully with defaults and with `PUID=1234 PGID=2345 TZ=UTC`. The native arm64 image built successfully and reported `sha256:27a06cd026c0445c69dfb91f50c512315e51f8dfbeba65a2627c62d3a4ea000a` (218,820,223 bytes, configured user `1000:1000`). A container runtime check confirmed UID/GID `1000:1000` and the Zagreb UTC offset. This is a local content ID, not a published registry digest.
