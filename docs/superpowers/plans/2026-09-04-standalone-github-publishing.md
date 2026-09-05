# Standalone GitHub and Container Publishing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extract `subsyncd` into a self-contained private GitHub repository and publish verified `linux/amd64` plus `linux/arm64` images to private GHCR on every successful `main` push.

**Architecture:** Prepare and verify all standalone-root files inside the existing `subsyncd/` subtree, then preserve subsystem history with `git subtree split`. A two-job GitHub Actions workflow gates a Buildx multi-platform publication on Go tests, vet, and tagged end-to-end tests; Compose consumes the private GHCR image by tag.

**Tech Stack:** Git/Git subtree, GitHub CLI, GitHub Actions, Go 1.27, Docker Buildx/QEMU, GitHub Container Registry, Docker Compose, actionlint v1.7.12.

**Spec:** `docs/superpowers/specs/2026-09-04-standalone-github-publishing-design.md`

## Global Constraints

- The destination repository is private `github.com/tomislav/subsyncd`; the container package is private `ghcr.io/tomislav/subsyncd`.
- Treat paths in this plan as relative to the standalone `subsyncd/` root. Before extraction, run implementation commands from `/Users/tomislav/Development/arr-stack/subsyncd`.
- Do not modify `/Users/tomislav/Development/arr-stack/docker-compose.yml`.
- Preserve the existing subsystem history with `git subtree split`; do not initialize an unrelated history or rewrite the parent branch.
- The standalone default branch is `main`; `/Users/tomislav/Development/subsyncd` becomes the canonical local clone after publication.
- Every successful `main` workflow publishes `latest` and `sha-<short-commit>`.
- A stable `vMAJOR.MINOR.PATCH` tag additionally publishes `MAJOR.MINOR.PATCH`, `MAJOR.MINOR`, and `MAJOR`; prereleases publish only the full prerelease version and SHA.
- Manual runs publish SHA and publish `latest` only when dispatched from `main`.
- Images target exactly `linux/amd64` and `linux/arm64`, with provenance and SBOM attestations.
- Verification must pass before image publication. Provider contract tests remain credential-gated and outside this workflow.
- Use only the ephemeral `GITHUB_TOKEN` for workflow GHCR writes. Never commit or print a personal access token.
- The standalone Compose example defaults to `latest` and accepts only a tag through `SUBSYNCD_IMAGE_TAG`.
- Keep the parent checkout recoverable. Repository creation, push, clone, or workflow failure must not delete branches, files, or remotes.
- GitHub CLI authentication is currently invalid. Stop at the publication boundary for interactive `gh auth login` if it remains invalid.

---

## File map

```text
subsyncd/
├── .github/workflows/container.yml                       # verify and publish multi-architecture GHCR images
├── .gitignore                                            # standalone runtime, credential, and build exclusions
├── AGENTS.md                                             # self-contained contributor/agent entry points
├── compose.example.yml                                   # private GHCR deployment definition
├── README.md                                             # private pull, tags, Compose, and local-build instructions
├── docs/implementation-status.md                         # implementation and publication handoff ledger
├── docs/superpowers/specs/2026-09-04-standalone-github-publishing-design.md
└── docs/superpowers/plans/2026-09-04-standalone-github-publishing.md
```

No application package or database schema changes are required.

### Task 1: Make the subtree self-contained

**Files:**
- Create: `.gitignore`
- Modify: `AGENTS.md`
- Existing: `docs/implementation-status.md`
- Existing: `docs/providers.md`
- Existing: `docs/operations.md`
- Existing: `docs/references/silo.md`
- Existing: `docs/release-notes.md`

**Interfaces:**
- Consumes: the current `subsyncd/` subtree and its in-tree documentation.
- Produces: a repository root that excludes local state and whose contributor guide resolves entirely inside the standalone checkout.

- [ ] **Step 1: Prove the current subtree is not standalone-safe**

Run from `/Users/tomislav/Development/arr-stack/subsyncd`:

```bash
test -f .gitignore
```

Expected: nonzero exit because the subtree has no root `.gitignore`.

Run:

```bash
rg -n '\.\./docs|Hearing-impaired subtitles are allowed by default' AGENTS.md
```

Expected: matches for the two parent-relative design/plan paths and the superseded hearing-impaired default.

- [ ] **Step 2: Add the standalone ignore contract**

Create `.gitignore` with exactly:

```gitignore
.DS_Store
.env
.env.*
!.env.example

/subsyncd
/coverage.out
/config/
/data/
/cache/
/media/

*.db
*.db-shm
*.db-wal
*.log
```

This keeps the compiled binary, Compose credentials, runtime configuration, SQLite state, caches, and sample mount data out of the standalone repository.

- [ ] **Step 3: Repair the contributor-guide entry points**

Replace the opening document list in `AGENTS.md` with:

```markdown
Read these documents before changing behavior:

1. `docs/implementation-status.md` for the behavior actually shipped and the resumable handoff ledger
2. `docs/providers.md` for provider, scoring, scheduling, and upgrade behavior
3. `docs/operations.md` for deployment, filesystem, LAPSE, webhook, and recovery behavior
4. `docs/references/silo.md` before changing Silo notification behavior
5. `docs/superpowers/specs/2026-09-04-standalone-github-publishing-design.md` and its matching plan before changing repository or image publication

`AGENTS.md` and `docs/implementation-status.md` are the authoritative entry points for current behavior. Historical design material that lived outside the original `subsyncd/` subtree is not required by the standalone repository.
```

Delete the earlier invariant that says hearing-impaired subtitles are allowed by default. Retain the later correct invariant verbatim:

```markdown
- Hearing-impaired/SDH tracks and candidates are disallowed by default. Only an explicit `allow_hearing_impaired: true` makes them satisfy inventory or remain eligible during candidate scoring.
```

- [ ] **Step 4: Verify every documented local path resolves**

Run:

```bash
test -r docs/implementation-status.md
test -r docs/providers.md
test -r docs/operations.md
test -r docs/references/silo.md
test -r docs/superpowers/specs/2026-09-04-standalone-github-publishing-design.md
test -r docs/superpowers/plans/2026-09-04-standalone-github-publishing.md
```

Expected: every command exits 0.

Run:

```bash
rg -n '\.\./docs|Hearing-impaired subtitles are allowed by default' AGENTS.md
```

Expected: exit 1 with no matches.

- [ ] **Step 5: Commit the standalone-root preparation**

From `/Users/tomislav/Development/arr-stack`:

```bash
git add subsyncd/.gitignore subsyncd/AGENTS.md subsyncd/docs/superpowers
git commit -m "chore: prepare standalone subsyncd tree"
```

### Task 2: Switch standalone Compose and README to private GHCR

**Files:**
- Modify: `compose.example.yml`
- Modify: `README.md`

**Interfaces:**
- Consumes: private image name `ghcr.io/tomislav/subsyncd` and the existing rootless Compose security/mount contract.
- Produces: `SUBSYNCD_IMAGE_TAG` deployment selection and operator instructions for authenticated private pulls.

- [ ] **Step 1: Capture the failing Compose image contract**

Run from the standalone root:

```bash
docker compose -f compose.example.yml config --images
```

Expected current output:

```text
subsyncd:local
```

Run:

```bash
SUBSYNCD_IMAGE_TAG=sha-0123456 docker compose -f compose.example.yml config --images
```

Expected current output remains `subsyncd:local`, proving the requested tag is ignored.

- [ ] **Step 2: Replace the local Compose build with the private image**

At the beginning of the `subsyncd` service in `compose.example.yml`, replace:

```yaml
    image: subsyncd:local
    build:
      context: .
      args:
        VERSION: dev
```

with:

```yaml
    image: ghcr.io/tomislav/subsyncd:${SUBSYNCD_IMAGE_TAG:-latest}
```

Do not change `user`, `TZ`, tmpfs UID/GID, read-only mode, mounts, health check, limits, or the external `media` network.

- [ ] **Step 3: Update README deployment instructions**

Add a `Private container image` section before `Quick start with Compose` with these contracts:

````markdown
## Private container image

GitHub Actions publishes `ghcr.io/tomislav/subsyncd` for `linux/amd64` and `linux/arm64`. The repository and package are private, so each Docker host must authenticate with a GitHub token that can read packages:

```bash
docker login ghcr.io --username tomislav
```

Enter the token through Docker's password prompt; do not put it in this repository or in a command argument.

Successful pushes to `main` publish `latest` and an immutable `sha-<commit>` tag. Stable `v*` tags also publish semantic-version aliases; prereleases publish only their full version and SHA. Set `SUBSYNCD_IMAGE_TAG` to pin Compose to a version or immutable SHA.
````

Change Quick Start step 1 to copy `config.example.yaml` to `config/config.yaml`, keep the credential and PUID/PGID/TZ steps, and replace the build/start commands with:

```bash
docker compose -f compose.example.yml pull
docker compose -f compose.example.yml up -d
docker compose -f compose.example.yml exec subsyncd subsyncd doctor --config /config/config.yaml
```

Change the production-image paragraph so it names `ghcr.io/tomislav/subsyncd`, both supported platforms, and the `latest` default. Preserve the Debian/LAPSE/rootless explanation.

Under `Native build`, retain the Go commands and add this explicit local image command:

```bash
docker build --build-arg VERSION=dev -t subsyncd:local .
```

Remove the root `arr-stack` profile command because it does not belong in the standalone repository. Do not add or edit the parent Compose file.

- [ ] **Step 4: Verify default and immutable Compose tags**

Run:

```bash
docker compose -f compose.example.yml config --images
```

Expected exact output:

```text
ghcr.io/tomislav/subsyncd:latest
```

Run:

```bash
SUBSYNCD_IMAGE_TAG=sha-0123456 docker compose -f compose.example.yml config --images
```

Expected exact output:

```text
ghcr.io/tomislav/subsyncd:sha-0123456
```

Run:

```bash
PUID=1234 PGID=2345 TZ=UTC docker compose -f compose.example.yml config
```

Expected: `image` remains GHCR, `user` is `1234:2345`, `/tmp` has `uid=1234,gid=2345`, `TZ` is `UTC`, the root filesystem is read-only, all capabilities are dropped, and the existing volumes/network remain present.

- [ ] **Step 5: Commit the deployment documentation**

From the parent repository:

```bash
git add subsyncd/compose.example.yml subsyncd/README.md
git commit -m "docs: deploy subsyncd from private GHCR"
```

### Task 3: Add verified multi-architecture publication

**Files:**
- Create: `.github/workflows/container.yml`

**Interfaces:**
- Consumes: Go 1.27 tests, the root `Dockerfile`, GHCR `GITHUB_TOKEN`, and Git refs.
- Produces: private multi-platform manifests tagged by main/SHA/semver with OCI metadata, provenance, and SBOM.

- [ ] **Step 1: Install the workflow validator outside the repository**

Run from the standalone root:

```bash
GOBIN=/tmp/subsyncd-actionlint go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
```

Expected: `/tmp/subsyncd-actionlint/actionlint` exists; no new repository file is created.

- [ ] **Step 2: Verify the workflow contract is currently absent**

Run:

```bash
/tmp/subsyncd-actionlint/actionlint .github/workflows/container.yml
```

Expected: nonzero exit because `container.yml` does not exist.

- [ ] **Step 3: Create the publication workflow**

Create `.github/workflows/container.yml` with exactly this structure and immutable action pins:

```yaml
name: Verify and publish container

on:
  push:
    branches:
      - main
    tags:
      - "v*"
  workflow_dispatch:

permissions:
  contents: read

concurrency:
  group: container-${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: ${{ github.ref == 'refs/heads/main' }}

env:
  IMAGE_NAME: ghcr.io/tomislav/subsyncd

jobs:
  verify:
    name: Verify
    runs-on: ubuntu-24.04
    steps:
      - name: Check out source
        uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
      - name: Set up Go
        uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7
        with:
          go-version: "1.27.0"
          cache-dependency-path: go.sum
      - name: Test with race detector
        run: go test ./... -race -count=1
      - name: Vet
        run: go vet ./...
      - name: Run tagged end-to-end tests
        run: go test ./test/e2e -tags=e2e -race -count=1

  publish:
    name: Publish multi-architecture image
    needs: verify
    runs-on: ubuntu-24.04
    permissions:
      contents: read
      packages: write
      attestations: write
      id-token: write
    steps:
      - name: Check out source
        uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
      - name: Set up QEMU
        uses: docker/setup-qemu-action@1f40c72289eff860ee54a304f1438e3cff362e0a # v4
      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@37fe631027851001ddb9b187196cc803df7f5f0e # v4
      - name: Log in to GHCR
        uses: docker/login-action@dbcb813823bdd20940b903addbd779551569679f # v4
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Generate image metadata
        id: meta
        uses: docker/metadata-action@dc802804100637a589fabce1cb79ff13a1411302 # v6
        with:
          images: ${{ env.IMAGE_NAME }}
          flavor: latest=auto
          tags: |
            type=raw,value=latest,enable={{is_default_branch}},priority=200
            type=sha,priority=300
            type=semver,pattern={{version}},priority=900
            type=semver,pattern={{major}}.{{minor}},priority=800
            type=semver,pattern={{major}},priority=700
      - name: Build and push
        uses: docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a # v7
        with:
          context: .
          file: ./Dockerfile
          platforms: linux/amd64,linux/arm64
          push: true
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
          build-args: |
            VERSION=${{ steps.meta.outputs.version }}
          cache-from: type=gha
          cache-to: type=gha,mode=max
          provenance: mode=max
          sbom: true
```

The SHA tag has priority 300 over raw `latest` at `main`, so the binary version becomes `sha-<short-commit>`. Stable semver has higher priority and becomes the binary version for stable tags. Docker metadata-action suppresses major/minor/latest aliases for prerelease semver tags.

- [ ] **Step 4: Validate workflow syntax and shell expressions**

Run:

```bash
/tmp/subsyncd-actionlint/actionlint .github/workflows/container.yml
```

Expected: exit 0 with no diagnostics.

- [ ] **Step 5: Run the exact verification commands used by Actions**

Run separately:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
```

Expected: all commands exit 0. Grant loopback permission only because local tests bind fake HTTP servers.

- [ ] **Step 6: Commit the workflow**

From the parent repository:

```bash
git add subsyncd/.github/workflows/container.yml
git commit -m "ci: publish verified multiarch images"
```

### Task 4: Perform the pre-publication gate and split history

**Files:**
- Modify: `docs/implementation-status.md`
- Verify: every tracked standalone file and the parent `docker-compose.yml`

**Interfaces:**
- Consumes: the locally committed standalone-safe subtree.
- Produces: reviewed branch `publish/subsyncd-main` whose tree is ready to become GitHub `main`.

- [ ] **Step 1: Scan the current subtree and relevant history for credential forms**

From `/Users/tomislav/Development/arr-stack`, run:

```bash
git grep -n -I -E 'ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16}|BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY' -- subsyncd
```

Expected: exit 1 with no matches.

Run:

```bash
git log --all -G'ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16}|BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY' -- subsyncd
```

Expected: no commits. Inspect `git status --short --untracked-files=all` and ensure no local configuration, `.env`, database, media, or cache path is staged.

- [ ] **Step 2: Run the complete local verification gate**

From the standalone root, run separately:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
/tmp/subsyncd-actionlint/actionlint .github/workflows/container.yml
docker compose -f compose.example.yml config --quiet
```

Expected: every command exits 0.

- [ ] **Step 3: Build and smoke-test the native image**

Run:

```bash
docker build --build-arg VERSION=prepublish -t subsyncd:prepublish .
docker run --rm subsyncd:prepublish --version
```

Expected final output includes:

```text
subsyncd prepublish
```

- [ ] **Step 4: Prove the parent Compose file was not changed**

From the parent repository, run:

```bash
git diff --exit-code 905ac38..HEAD -- docker-compose.yml
```

Expected: exit 0 with no diff.

- [ ] **Step 5: Record locally completed publishing work**

Update the current-state summary and add a `Follow-up — standalone GitHub publishing` section to `docs/implementation-status.md`. Record:

- the commits from Tasks 1–3;
- the standalone path and private repository/image names;
- the exact action pins and tag policy;
- the Compose/README/agent-document changes;
- the full local verification commands and results;
- that external repository/image publication remains pending GitHub authentication.

Then commit from the parent repository:

```bash
git add subsyncd/docs/implementation-status.md
git commit -m "docs: record standalone publication setup"
```

- [ ] **Step 6: Create the history-preserving standalone branch**

First confirm the branch name is unused:

```bash
git show-ref --verify --quiet refs/heads/publish/subsyncd-main
```

Expected: exit 1. If it exists, inspect it and stop rather than overwrite it.

Run:

```bash
git subtree split --prefix=subsyncd --branch publish/subsyncd-main
```

Expected: a new branch whose root is the former `subsyncd/` directory.

- [ ] **Step 7: Inspect the split tree before any external write**

Run:

```bash
git ls-tree --name-only publish/subsyncd-main
git show publish/subsyncd-main:AGENTS.md
git show publish/subsyncd-main:.gitignore
git show publish/subsyncd-main:.github/workflows/container.yml
```

Expected: root entries include `.github`, `.gitignore`, `AGENTS.md`, `Dockerfile`, `README.md`, `compose.example.yml`, `docs`, `go.mod`, and source/test directories. `AGENTS.md` contains no parent-relative documentation path, and ignored runtime paths are absent from the tree.

### Task 5: Create and verify the private GitHub repository

**Files:**
- External create: `github.com/tomislav/subsyncd`
- External create: `ghcr.io/tomislav/subsyncd`
- Local create: `/Users/tomislav/Development/subsyncd`

**Interfaces:**
- Consumes: `publish/subsyncd-main`, valid `gh` authentication, and GitHub-hosted Actions/GHCR.
- Produces: private canonical repository, successful first workflow, private dual-platform image, and canonical local clone.

- [ ] **Step 1: Restore GitHub CLI authentication without exposing a token**

Run:

```bash
gh auth status -h github.com
```

If it still reports the invalid `tomislav` token, pause and run the interactive browser flow:

```bash
gh auth login -h github.com -w
```

The user completes GitHub authorization in the browser. Rerun `gh auth status -h github.com` and continue only when `tomislav` is authenticated with repository, workflow, and package access. Never echo the token.

If `read:packages` or `workflow` is absent, request only those additional scopes and configure Git to use the renewed credential:

```bash
gh auth refresh -h github.com -s read:packages,workflow
gh auth setup-git
gh auth status -h github.com
```

- [ ] **Step 2: Ensure the destination does not already exist**

Run:

```bash
gh repo view tomislav/subsyncd --json nameWithOwner,isPrivate,defaultBranchRef
```

Expected: repository-not-found. If a repository exists, stop and ask whether to reuse it; do not overwrite or force-push it.

- [ ] **Step 3: Create the empty private repository**

Run:

```bash
gh repo create tomislav/subsyncd --private --description "Headless subtitle acquisition, scoring, synchronization, and upgrade service"
```

Expected: `https://github.com/tomislav/subsyncd` is created without a generated README, license, or `.gitignore` commit.

- [ ] **Step 4: Push the split history as main and set the default branch**

From `/Users/tomislav/Development/arr-stack`, run:

```bash
git push https://github.com/tomislav/subsyncd.git publish/subsyncd-main:refs/heads/main
gh repo edit tomislav/subsyncd --default-branch main
gh repo view tomislav/subsyncd --json nameWithOwner,isPrivate,defaultBranchRef,url
```

Expected: `nameWithOwner` is `tomislav/subsyncd`, `isPrivate` is `true`, and the default branch name is `main`.

- [ ] **Step 5: Create the canonical standalone clone**

Run:

```bash
test ! -e /Users/tomislav/Development/subsyncd
```

Expected: exit 0. If the path exists, stop and inspect it instead of deleting or overwriting it.

Run:

```bash
git clone https://github.com/tomislav/subsyncd.git /Users/tomislav/Development/subsyncd
```

From the clone, run:

```bash
git status --short
git branch --show-current
git remote -v
test -r .github/workflows/container.yml
test -r docs/implementation-status.md
test -r docs/superpowers/specs/2026-09-04-standalone-github-publishing-design.md
test -r docs/superpowers/plans/2026-09-04-standalone-github-publishing.md
```

Expected: clean `main`, `origin` points to `tomislav/subsyncd`, and every file check exits 0.

- [ ] **Step 6: Wait for the first workflow instead of assuming publication**

Run:

```bash
release_run_id="$(gh run list --repo tomislav/subsyncd --workflow container.yml --limit 1 --json databaseId --jq '.[0].databaseId')"
gh run watch "$release_run_id" --repo tomislav/subsyncd --exit-status
gh run view "$release_run_id" --repo tomislav/subsyncd --json status,conclusion,headSha,url,jobs
```

Expected: conclusion `success`; both `Verify` and `Publish multi-architecture image` succeed. If not, inspect logs, fix the source through the standalone clone, push normally, and wait for the replacement run—never bypass verification.

- [ ] **Step 7: Verify package privacy, association, tags, and platforms**

Run:

```bash
gh api /user/packages/container/subsyncd --jq '{name: .name, visibility: .visibility, repository: .repository.full_name}'
```

Expected: name `subsyncd`, visibility `private`, repository `tomislav/subsyncd`. If privacy or association differs, stop and correct it in GitHub package settings before pulling.

Authenticate Docker without placing the token in an argument:

```bash
gh auth token | docker login ghcr.io --username tomislav --password-stdin
```

From the standalone clone, run:

```bash
docker buildx imagetools inspect ghcr.io/tomislav/subsyncd:latest
image_sha_tag="sha-$(git rev-parse --short=7 HEAD)"
docker buildx imagetools inspect "ghcr.io/tomislav/subsyncd:${image_sha_tag}"
docker run --rm "ghcr.io/tomislav/subsyncd:${image_sha_tag}" --version
```

Expected: both tags resolve to the same manifest; the manifest lists `linux/amd64` and `linux/arm64`; version output identifies `${image_sha_tag}`.

### Task 6: Record publication evidence and verify the final main image

**Files:**
- Modify in canonical clone: `docs/implementation-status.md`

**Interfaces:**
- Consumes: the successful first Actions run, private package metadata, immutable manifest digest, and canonical clone.
- Produces: durable publication handoff, a final clean `main`, and a verified image for the final documentation commit.

- [ ] **Step 1: Capture immutable first-publication evidence**

From `/Users/tomislav/Development/subsyncd`, run:

```bash
gh run view "$release_run_id" --repo tomislav/subsyncd --json databaseId,headSha,url,conclusion
docker buildx imagetools inspect "ghcr.io/tomislav/subsyncd:${image_sha_tag}"
gh api /user/packages/container/subsyncd --jq '{visibility: .visibility, repository: .repository.full_name}'
```

Record the exact run URL/head SHA, SHA tag, manifest digest, both platforms, package privacy/association, and version-smoke result from these commands.

- [ ] **Step 2: Update the implementation ledger**

Replace the pending-publication statement in `docs/implementation-status.md` with the actual result. Include:

- private repository and GHCR URLs;
- default branch `main` and canonical local path;
- first successful workflow URL and head SHA;
- immutable `sha-*` tag and manifest-list digest;
- `linux/amd64` and `linux/arm64` confirmation;
- package visibility `private` and repository association;
- container `--version` output;
- GitHub CLI authentication was renewed interactively without storing a token;
- the parent `arr-stack/docker-compose.yml` remained unchanged.

- [ ] **Step 3: Commit and push the handoff ledger**

Run:

```bash
git add docs/implementation-status.md
git commit -m "docs: record private GitHub publication"
git push origin main
```

Expected: a normal fast-forward push that triggers one final workflow for the documentation commit.

- [ ] **Step 4: Wait for and verify the final workflow**

Run:

```bash
final_run_id="$(gh run list --repo tomislav/subsyncd --workflow container.yml --limit 1 --json databaseId --jq '.[0].databaseId')"
gh run watch "$final_run_id" --repo tomislav/subsyncd --exit-status
gh run view "$final_run_id" --repo tomislav/subsyncd --json status,conclusion,headSha,url,jobs
```

Expected: conclusion `success` and head SHA equals the current standalone `HEAD`.

Run:

```bash
final_image_sha_tag="sha-$(git rev-parse --short=7 HEAD)"
docker buildx imagetools inspect ghcr.io/tomislav/subsyncd:latest
docker buildx imagetools inspect "ghcr.io/tomislav/subsyncd:${final_image_sha_tag}"
docker run --rm "ghcr.io/tomislav/subsyncd:${final_image_sha_tag}" --version
```

Expected: `latest` and the final SHA tag resolve to the final manifest, both target platforms are present, and version output identifies `${final_image_sha_tag}`.

- [ ] **Step 5: Perform final repository checks**

From the canonical clone, run separately:

```bash
git status --short
git branch --show-current
git remote -v
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
/tmp/subsyncd-actionlint/actionlint .github/workflows/container.yml
docker compose -f compose.example.yml config --quiet
git diff --check
```

Expected: clean `main`, correct `origin`, and every verification command exits 0.

- [ ] **Step 6: Report exact published artifacts**

Report the private repository URL, workflow URL, GHCR image, final `latest` manifest digest, final immutable SHA tag, both platforms, canonical local path, commits, and verification results. State explicitly that the parent `arr-stack` Compose file was not changed and that no semantic-version tag or GitHub Release was created.
