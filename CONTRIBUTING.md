# Contributing to subsyncd

How to build subsyncd, run its tests, and get a change merged and released. How the code works is in the [developer documentation](docs/development/README.md); coding agents also read [AGENTS.md](AGENTS.md).

- [Set up](#set-up)
- [Build from source](#build-from-source)
- [Tests](#tests)
- [Working rules](#working-rules)
- [Commit messages](#commit-messages)
- [CI](#ci)
- [Dockerfiles](#dockerfiles)
- [Releases](#releases)

## Set up

- **Go 1.27.1**, the version in `go.mod`. CI and both Dockerfiles pin the same version; change all of them together.
- **FFprobe** and **LAPSE v2.3.1** to run the service natively. The tests do not need them.
- Enable the commit-message hook once per clone:

  ```sh
  git config core.hooksPath .githooks
  ```

## Build from source

```sh
go build -trimpath -o subsyncd ./cmd/subsyncd
./subsyncd serve --config ./config/config.yaml
```

- Use absolute paths in the configuration, including `sync.lapse_path`.
- Native builds and locally built images have no embedded OpenSubtitles application key, so set `api_key` on the OpenSubtitles provider. Published images include one.
- Once the service has started and created its database, run `./subsyncd doctor --config ./config/config.yaml` from another terminal. `doctor` opens the database read-only and never creates or migrates it.
- `./subsyncd --help` lists the commands; `./subsyncd COMMAND --help` describes one.

Build a local image with the root Dockerfile:

```sh
docker build --build-arg VERSION=dev -t subsyncd:local .
```

## Tests

| Suite | Command | Contacts |
| --- | --- | --- |
| Unit and integration | `go test ./... -race -count=1` | Nothing: local fixtures and fake servers |
| Vet | `go vet ./...` | |
| End-to-end | `go test ./test/e2e -tags=e2e -race -count=1` | Nothing: fake Arr and provider servers, stubbed LAPSE and FFprobe runners, temporary media roots |
| Provider contracts | `go test ./test/providercontract -tags=provider_contract -v -count=1` | The real provider APIs |

Run the affected package with `-race` while you work, and all three local suites before you push; CI runs exactly those. `gofmt -l .` must print nothing.

### Provider contract tests

The `provider_contract` tests check that the live provider APIs still match the adapters. They are never part of CI. Each subtest skips unless its variables are set:

| Test | Variables |
| --- | --- |
| OpenSubtitles | `OPENSUBTITLES_API_KEY`, `OPENSUBTITLES_USERNAME`, `OPENSUBTITLES_PASSWORD` |
| SubDL | `SUBDL_API_KEY` |
| SubSource | `SUBSOURCE_API_KEY` |
| Titlovi | `TITLOVI_USERNAME`, `TITLOVI_PASSWORD` |
| Gestdown | `GESTDOWN_LIVE_TEST=1` (keyless, so it needs an explicit opt-in) |

The credentialed tests run one bounded broad search for a fixed movie and download nothing. The Gestdown test searches a fixed episode, downloads one subtitle, and checks extraction and episode selection. For example:

```sh
env GESTDOWN_LIVE_TEST=1 go test ./test/providercontract -tags=provider_contract -run '^TestGestdownLiveSearchDownload$' -v -count=1
```

Use your own accounts, and never commit their values or the responses.

## Working rules

- **Tests first.** Add one focused failing test, confirm it fails for the expected reason, make the smallest change that passes, then run the package with `-race`.
- **Fixtures never contact real services.** Ordinary and e2e tests use sanitized fixtures and local fake servers; they must never reach Arr, subtitle providers or Silo.
- **Keep docs in sync.** Update the operator page and the developer reference a change affects in the same commit. Event names and fields go in the [event catalogue](docs/development/logging.md#event-catalogue); schedules in [Scheduling](docs/development/providers/scheduling.md); schema changes in the [migration index](docs/development/persistence.md#migrations).
- **No history files.** Do not add task ledgers, plans, review transcripts or status files; Git history is the record.
- **No sensitive data.** Never put secrets, production hostnames or paths, provider responses, or real media metadata in code, fixtures, logs, commits or documentation.
- **Preserve unrelated working-tree changes.**
- **Migrations** follow [Adding a migration](docs/development/persistence.md#adding-a-migration); providers follow [Adding a provider](docs/development/providers/adapters.md#adding-a-provider).

## Changes and reviews

- **Describe behaviour as a timeline or example.** "Installed at T: checks at T+6h, T+12h, T+24h, T+3d" cannot be misread; "steps 6h, 12h, 24h, 3d" can (from install, or from the previous check?).
- **"Same as X" means reuse X.** Call the existing code or share its constants; a copied list drifts.
- **Acceptance tests come from the request.** Write at least one test in the request's own terms (the timeline or example), not only from your reading of it. For a "same as X" change, compare against X directly.
- **Say when you interpreted.** If you chose one reading of an ambiguous request, or deviated from it, say so explicitly in the commit body or pull request. A reviewer treats that as a question to settle, not a footnote.
- **Review against the request.** A review checks that the change does what was asked, not only that it is internally correct, and that code, comments and docs agree. A comment that disagrees with the code ("about a day" over a 2.5-hour schedule) is a bug in one of them.

## Commit messages

Use Conventional Commits: `<type>(<scope>): <summary>`.

- **Types**: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `build`, `ci`, `chore`. A `!` before the colon marks a breaking change.
- **Scope** (optional) is the subsystem, usually the package: `cli`, `control`, `app`, `store`, `workflow`, `worker`, `catalog`, `provider`, `syncer`, `notifier`, `e2e`, `docs`, `docker`. Lowercase letters, digits and hyphens.
- **Summary**: imperative, lowercase, no trailing period, and the whole subject at most 72 characters. Put the detail in the body.
- Older history uses sentence-style subjects; don't imitate it.

Examples:

```text
fix(control): bound socket drain on shutdown
feat(notifier): ask Sonarr and Radarr to rescan after sidecar changes
docs: split the developer references
```

`.githooks/commit-msg` enforces the format and length. It lets `Merge`, `Revert`, `fixup!`, `squash!` and `amend!` subjects through.

## CI

`.github/workflows/container.yml` runs on pushes to `main`, on `v*` tags, and on manual dispatch. A newer push to `main` cancels a running one.

**Verify** (every run):

1. `scripts/verify-release-dockerfile.sh`: checks that `Dockerfile.release` is exactly the root `Dockerfile` with the permitted secret-linking change.
2. `go test ./... -race -count=1`
3. `go vet ./...`
4. `go test ./test/e2e -tags=e2e -race -count=1`

**Publish** (after verify passes):

- Builds `Dockerfile.release` for `linux/amd64` and `linux/arm64` with QEMU and Buildx and pushes to `ghcr.io/tomislav/subsyncd`.
- Tags: `sha-<commit>` on every build; `edge` on every push to `main`; for a `v*` tag the semver tags `X.Y.Z`, `X.Y` and `X`, and also `latest` (through `flavor: latest=auto`). A pre-release tag such as `v1.3.0-rc.1` publishes only `1.3.0-rc.1` and `sha-<commit>`. Only a release moves `latest`. The version label and `VERSION` build argument are `X.Y.Z` for a `v*` tag and `sha-<commit>` on `main`.
- Requires the repository secret `OPENSUBTITLES_API_KEY`, passed as a BuildKit secret and embedded as the application key; the job fails early when it is empty.
- Publishes SBOM and provenance attestations (`provenance: mode=max`, `sbom: true`).

**Release** (after publish, `v*` tags only):

- `scripts/release-notes.sh` writes the notes: the image and its digest, the `feat` and `fix` subjects since the previous stable tag (commits with `!` under Breaking changes), and a back-up warning when a file was added under `internal/store/migrations/`. Pre-release tags are never the baseline, so an rc and its final release list the same changes.
- `gh release create` publishes a GitHub release for the tag, marked as a pre-release when the tag has a `-` suffix.

## Dockerfiles

| File | Used by | Difference |
| --- | --- | --- |
| `Dockerfile` | Local builds | No OpenSubtitles application key |
| `Dockerfile.release` | CI publishing | Mounts the `opensubtitles_api_key` build secret, validates it, and links it into `internal/provider/opensubtitles.builtInAPIKey` |

Change `Dockerfile` first and mirror the change in `Dockerfile.release`; `scripts/verify-release-dockerfile.sh` fails on any other difference.

Both images:

- use a digest-pinned Ubuntu 26.04 LTS base for the LAPSE download and runtime stages, because LAPSE 2.3.1's amd64 build needs glibc 2.43; the runtime removes the base image's `ubuntu` user before creating UID/GID 1000;
- build a static `ffprobe` from a checksummed FFmpeg source archive in a digest-pinned Alpine stage (`scripts/build-ffprobe.sh`), with only the local-file protocol and the demuxers, parsers and subtitle decoders inventory needs. The full `ffmpeg` command is not installed. The script checks the source digest and target architecture and writes the LGPL license and a CycloneDX inventory to `/usr/share/sbom/ffmpeg.cdx.json`;
- download LAPSE by version and per-architecture checksum; LAPSE keeps its own FFmpeg libraries, ONNX Runtime and Silero model, and `libstdc++6` stays a runtime dependency;
- check `ffprobe -version` and LAPSE's Silero load during the build;
- run as UID/GID 1000 with `/config`, `/data` and `/media` owned by it (the Compose example also sets `read_only: true`), a `/readyz` healthcheck, and `serve --config /config/config.yaml` as the default command.

When upgrading FFmpeg or LAPSE, change the version, checksums and runtime version labels together in both files. A LAPSE upgrade also changes `CompatibilityVersion` and needs a keep-or-move decision on the rejection baseline and a check of the binaries' glibc and CPU requirements ([Upgrading LAPSE](docs/development/providers/lapse.md#upgrading-lapse)). Keep the runtime rootless, keep the writable mounts limited to `/data`, the media roots and `/tmp`, and keep the Compose stop grace covering both 30-second shutdown windows.

## Releases

Versions follow [Versioning](docs/upgrade-and-backup.md#versioning): a major release for anything that breaks a working configuration, command, webhook or `--json` output, a minor one for features, a patch for fixes. Release notes come from commit subjects, so write them for operators.

1. Make sure `main` is green and the docs describe the release. Note any new migration: operators must back up before upgrading, because migrations are forward-only.
2. Release a commit that has already been published as `edge` and run on a real installation. Every push to `main` goes through the same verify and build jobs as a release, so `edge` is the release candidate. Tag that commit:

   ```sh
   git tag -a v1.2.3 -m v1.2.3 COMMIT
   git push origin v1.2.3
   ```

   The tag build rebuilds the commit from source; it does not copy the `edge` image, so the release image has its own digest.

   Push a release candidate first only when the tag path itself changed (the workflow's tag, label or release steps), or for a major release that others should try before `latest` moves. A candidate publishes `:1.2.3-rc.1` and a GitHub pre-release without moving `latest`, `1` or `1.2`:

   ```sh
   git tag -a v1.2.3-rc.1 -m v1.2.3-rc.1 COMMIT
   git push origin v1.2.3-rc.1
   ```

3. CI verifies and publishes `ghcr.io/tomislav/subsyncd:1.2.3`, `:1.2`, `:1`, `:sha-<commit>` and `:latest`, then creates the GitHub release with generated notes. Edit the notes on GitHub if they need context.
