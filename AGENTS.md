# subsyncd contributor guide

Read these documents before changing behavior:

1. `../docs/superpowers/specs/2026-09-03-focused-subtitle-service-design.md`
2. `../docs/superpowers/plans/2026-09-04-focused-subtitle-service.md`
3. `docs/implementation-status.md`
4. `docs/references/bazarr.md` for the task being implemented

The design is authoritative. Bazarr commit `da73aeaf5e4d89ad86c8d559d3abd0e4129b24b2` is a GPL-3.0 behavioral reference only. Do not copy, vendor, execute, or translate its Python implementation or fixtures.

Use test-driven development: add one focused failing test, confirm the expected failure, implement the smallest production change, then run the affected package with `-race`. Ordinary tests must use sanitized fixtures and local fake servers; they must never contact Arr applications or subtitle providers.

Important invariants:

- Canonical language identities are BCP 47 tags.
- Embedded subtitle inventory is fingerprint-cached; sidecars are scanned live before searches.
- Providers are compiled-in adapters behind the common interface.
- Remote provider cooldowns are persisted and release worker leases; provider code must not sleep through them.
- A season-pack member must be uniquely identified. Never select the first arbitrary archive member.
- LAPSE is the only synchronization engine. Non-exact candidates require its `solid` verdict.
- Only unchanged files owned by this service may be upgraded.
- Every media write is root-contained, atomic, checksum-recorded, and auditable.

Local verification uses writable caches:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
```

Update `docs/implementation-status.md` at every completed task boundary with the commit, adopted/tightened/rejected behavior, tests run, and next task.
