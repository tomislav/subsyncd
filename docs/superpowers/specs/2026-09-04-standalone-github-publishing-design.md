# Standalone GitHub and Container Publishing Design

**Status:** Approved for implementation planning

**Repository:** `github.com/tomislav/subsyncd` (private)

**Container:** `ghcr.io/tomislav/subsyncd` (private)

## Purpose

Publish `subsyncd` as a standalone private GitHub repository with its existing subsystem history, verify every published revision in GitHub Actions, and generate one multi-architecture container image that the standalone Compose example can consume directly.

The current `arr-stack` Compose file is outside this change. It remains untouched.

## Repository extraction and ownership

The current source lives below `subsyncd/` in the `arr-stack` Git repository. Implementation changes are made and verified there first. A subtree split then rewrites that directory as the root of a standalone `main` branch while preserving commits that affected the subsystem.

The private GitHub repository is created without generated files or an initial commit. The split branch is pushed as `main`, and `/Users/tomislav/Development/subsyncd` is cloned from it as the new canonical local working copy. The existing `arr-stack` checkout and its embedded `subsyncd/` directory remain intact; this task does not delete or rewrite them.

Runtime directories, local configuration, databases, credentials, and caches remain excluded by `.gitignore` and `.dockerignore`. Before publication, inspect the complete standalone tree and history-visible configuration for secrets.

GitHub CLI authentication for the `tomislav` account must be valid before repository creation. The currently stored token is invalid, so implementation must pause at the external-publication boundary and request interactive re-authentication if it has not already been renewed. Authentication tokens must never be copied into project files or command output.

## Agent documentation

The standalone repository must be self-contained for future contributors and agents. `AGENTS.md` currently refers to design and plan files outside the `subsyncd/` subtree; those paths would break after extraction.

Before splitting, update `AGENTS.md` to point only at documentation shipped inside the standalone repository. Preserve the actual-behavior ledger, provider and Silo references, operator documentation, release notes, this publishing design, and its implementation plan. The historical parent-repository design and plan are not required runtime inputs; any still-authoritative invariant absent from the standalone documents must be moved into `AGENTS.md` or the implementation-status ledger before extraction.

## GitHub Actions workflow

Create `.github/workflows/container.yml` in the standalone tree. It runs for:

- pushes to `main`;
- Git tags matching `v*`;
- manual `workflow_dispatch` runs.

The workflow has two jobs.

### Verification job

The verification job uses the pinned Go 1.27 toolchain and writable runner caches. It runs:

```text
go test ./... -race -count=1
go vet ./...
go test ./test/e2e -tags=e2e -race -count=1
```

Ordinary and end-to-end tests use only local fakes. Credential-gated provider contract tests are not run by the publication workflow.

### Container job

The container job starts only after verification succeeds. It:

1. Checks out the exact triggering revision.
2. Configures QEMU and Docker Buildx.
3. Authenticates to GHCR using the ephemeral `GITHUB_TOKEN`.
4. Generates OCI labels and tags from the repository/ref.
5. Builds the existing root `Dockerfile` for `linux/amd64` and `linux/arm64`.
6. Pushes a manifest list plus per-platform images to GHCR.
7. Uses the GitHub Actions cache for reusable BuildKit layers.
8. Publishes provenance and an SBOM with the image.

Workflow permissions are minimal: `contents: read`, `packages: write`, and `attestations: write`/`id-token: write` only where required for provenance. No personal access token or registry password is stored as a repository secret.

Concurrency is grouped by workflow and Git ref. A newer `main` build may cancel an older in-progress `main` build, but version-tag builds are not cancelled by unrelated refs.

## Image tags

After a successful push to `main`, publish:

```text
ghcr.io/tomislav/subsyncd:latest
ghcr.io/tomislav/subsyncd:sha-<short-commit>
```

After a successful tag such as `v1.2.3`, additionally publish:

```text
ghcr.io/tomislav/subsyncd:1.2.3
ghcr.io/tomislav/subsyncd:1.2
ghcr.io/tomislav/subsyncd:1
```

The version tag also updates `latest` because the tagged commit is expected to be a tested `main` revision. A prerelease tag such as `v1.2.3-rc.1` publishes only its full prerelease version and commit SHA; it must not update the stable `1.2`, `1`, or `latest` aliases.

Manual workflow runs publish the commit SHA. They update `latest` only when run against `main`.

The Docker build receives a version string derived from the selected metadata tag so `subsyncd --version` and OCI labels identify the built revision. The immutable SHA tag is always available for rollback even though Compose defaults to `latest`.

## Standalone Compose example

Update `compose.example.yml` to remove the local `build:` section and use:

```yaml
image: ghcr.io/tomislav/subsyncd:${SUBSYNCD_IMAGE_TAG:-latest}
```

All existing rootless UID/GID interpolation, timezone, read-only filesystem, tmpfs ownership, mounts, health check, security options, resource limits, and networking remain unchanged.

The variable contains only the tag, not an arbitrary image reference. Operators can choose `latest`, a semantic version, or an immutable `sha-*` tag without editing the file.

## README changes

The standalone README becomes the GitHub landing page and documents:

- that the repository and GHCR package are private;
- authenticating Docker to `ghcr.io` with a token that has `read:packages`;
- pulling and starting the default `latest` image;
- pinning `SUBSYNCD_IMAGE_TAG` to a semantic version or immutable SHA;
- the `linux/amd64` and `linux/arm64` targets;
- the exact `main`, version-tag, prerelease, and manual publishing rules;
- retaining local Go tests and local Docker builds for development;
- where configuration and operations documentation lives.

Examples never include a real credential. They avoid placing a token directly in a command argument or committed environment file.

## Failure behavior

- Test or vet failure prevents every image push.
- A failure on either platform prevents publishing the multi-architecture manifest for that workflow run.
- Registry login/build/push failure leaves the previous tags untouched where GHCR supports atomic manifest publication; operators can continue using an immutable prior SHA.
- Repository creation or push failure does not delete the original `arr-stack` checkout or split branch.
- A failed standalone clone leaves the remote repository intact and can be retried.
- The GitHub Actions run must be observed through completion after the first push. A queued or running job is not publication success.

## Verification and acceptance

Before the external push:

- Run the complete race-enabled Go suite, vet, tagged end-to-end suite, and `git diff --check`.
- Render the Compose file with its default tag and with an explicit SHA-like tag; verify the resolved image and all rootless/security settings.
- Validate the workflow YAML and inspect generated Docker metadata/tag expressions.
- Build the local native container and verify `subsyncd --version` when practical.
- Review tracked files and relevant history/configuration for credentials.

After the external push:

- Confirm `github.com/tomislav/subsyncd` is private and its default branch is `main`.
- Confirm the standalone repository root contains the binary source, Dockerfile, Compose example, README, workflow, and self-contained agent documentation.
- Wait for the first GitHub Actions workflow to finish successfully.
- Confirm the GHCR package is private and associated with the repository.
- Confirm the published manifest contains both `linux/amd64` and `linux/arm64`.
- Confirm `latest` and the triggering immutable `sha-*` tag resolve to the expected manifest.
- Pull or inspect the published image and verify its reported version.
- Record the repository, workflow, package, manifest, and verification results in `docs/implementation-status.md` without storing credentials.

## Non-goals

- Changing the parent `arr-stack/docker-compose.yml`.
- Publishing a public repository or public container package.
- Creating a GitHub Release or an initial semantic-version tag.
- Automatically deploying the image to a server.
- Running credential-gated provider contract tests in CI.
- Adding Renovate, Dependabot, signing keys, release notes automation, or a general release-management framework.
