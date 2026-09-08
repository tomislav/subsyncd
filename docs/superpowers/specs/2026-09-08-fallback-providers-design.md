# Per-language fallback providers

Status: implemented and locally verified.

Languages retain ordered `providers` and optionally add ordered `fallback_providers`.
Primary providers are required. Unknown providers, duplicates within/across tiers,
and unsupported language routes fail startup validation. Existing configuration
retains its behavior.

Provider tier precedes match type: exhaust preferred cache/exact/broad acquisition
before fallback cache/exact/broad acquisition. Each broad tier has its own three
result shortlist and unchanged release-score tournament. Cached packs must belong
to the active tier. Do not contact fallback providers after a preferred installation.
Candidate-local failures and provider outages permit fallback; cancellation,
repository, stale-media, publication and rollback failures remain terminal.
Persist scored evidence from both attempted tiers without download references.

Store `fallback` on installation provenance atomically with the sidecar/outbox.
Release scores and subtitle filenames stay unchanged. An unchanged managed fallback
can be promoted to a currently preferred provider without a score delta, including
when the fallback was exact. Minimum score, identity and LAPSE requirements remain:
nonexact promotions are upgrades and require solid LAPSE under the normal policy.
Never replace a preferred installation with a fallback merely for its higher score.
Same-tier upgrades retain existing rules. Manual/modified subtitles remain protected.

Fallback installations get weekly upgrade checks even at exact hash or score 100.
An exhausted successful promotion search retains the installation and weekly check;
technical failure/cooldown retains ordinary retry handling. A successful fallback
installation after preferred cooldown checks again at the earlier future reset or
weekly check. Current configuration determines promotion eligibility (including
providers moved between tiers); the stored flag records installation-time provenance.
Manual successful searches persist nonterminal upgrade checks, reopening completed rows while preserving leases and earlier queued work.
Existing terminal exact installs are not proactively rescheduled on configuration
changes: manual search can reconsider them. No startup network/backfill is added.

Implementation uses two coordinators per language and a shared inventory check with
sequential acquisition passes. A request-local accumulator retains candidate rows.
SQLite migration 006 adds a default-false boolean; no legacy score rewriting.
Diagnostics show stored fallback status and include states from both provider tiers.

Verification uses sanitized local fakes, focused red/green tests, race-enabled full
suite, vet, tagged e2e, diff review. No production interaction or publication.
