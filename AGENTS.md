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
- Provider file hashes are calculated lazily, persisted by algorithm, and reusable only for an exact path/file-ID/size/mtime fingerprint.
- Providers are compiled-in adapters behind the common interface.
- Remote provider cooldowns are persisted and release worker leases; provider code must not sleep through them.
- All provider downloads pass through the common bounded extractor. ZIP, RAR, and plain subtitle payloads are accepted; traversal, links, nested archives, decompression-limit violations, invalid subtitle syntax, and ambiguous season-pack members fail closed.
- A season-pack member must be uniquely identified by provider evidence, episode/range/absolute tokens, or the strict episode-title rule. Never select the first arbitrary archive member.
- Pack-cache directories are content-addressed and immutable. Never persist candidate download references, follow cache symlinks, or delete paths outside the exact cache layout.
- Sonarr episode titles are persisted because they are part of strict pack-member evidence; schema changes must update both normal and event-transaction media upserts.
- LAPSE is the only synchronization engine. The compatibility baseline is LAPSE v2.0.5; non-exact candidates require its documented `solid` JSON verdict. Its strict weak verdicts return exit 2, and dry-run reports `written:true` to mean “would write.”
- Invoke LAPSE through argument arrays with `--json --strict --no-sidecar`; analysis also uses `--dry-run` on a private copy, while synchronization uses an explicit new `--output` plus `--no-backup`. Cancellation must kill the subprocess group.
- LAPSE JSON is an intentionally strict protocol boundary. When upgrading LAPSE, update the documented mode/reference/field allowlists and sanitized fixtures together after checking the official source contract.
- Only unchanged files owned by this service may be upgraded.
- Every media write is root-contained, atomic, checksum-recorded, and auditable.

Local verification uses writable caches:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
```

Update `docs/implementation-status.md` at every completed task boundary with the commit, adopted/tightened/rejected behavior, tests run, and next task.
