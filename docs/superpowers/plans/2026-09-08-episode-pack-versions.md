# Episode pack versions implementation plan

> **For agentic workers:** Use superpowers:executing-plans for inline execution. Use superpowers:requesting-code-review for independent read-only review.

**Goal:** Recognize dotted episode coordinates, evaluate explicit same-episode versions safely, and explain preparation outcomes at info level.

**Architecture:** Extend the shared parser and add a bounded alternative selector beside the strict single-member selector. Carry member-scoped sources and validated outputs through the existing workflow tournament and immutable cache lookup, keeping provider identity unchanged.

**Tech Stack:** Go 1.27.1, existing SQLite store, LAPSE v2.0.5 interface, synthetic ZIP fixtures.

**Spec:** `docs/superpowers/specs/2026-09-08-episode-pack-versions-design.md`

## Global constraints

Three broad provider results, at most three distinct explicit-episode versions per result. No guessed episode, exact-hash ambiguity bypass, additional LAPSE invocation for installation, technical rejection poisoning, cache mutation, production mutation or live provider tests.

## Steps

- [x] Add failing dotted-coordinate and complete-token tests in `internal/pack/separated_episode_test.go`. Update `select.go` token/range patterns and token boundaries; run pack race tests.
- [x] Add `SelectAlternatives` regression for two explicit versions, wrong/forced exclusion, exact ambiguity and more-than-three rejection. Implement selection and content dedup in `internal/pack/alternatives.go`, retaining `Select` behavior.
- [x] Add failing workflow regression requiring one download, two sync calls and the higher confidence output. Add `member_preparation.go`; preserve result decisions, source identity, private output allocation and tier fallback in `service.go`.
- [x] Add failing cache sibling regression. Extend `CachedMember` and `FindEligible` to verify/filter a bounded group; feed its accepted sources through the same preparation helper. Add member-scoped rejection signatures and deterministic exhaustion aggregation.
- [x] Prove info-level archive rejection and winner/member correlation fail before adding the events. Keep reason codes bounded and payload details out of logs.
- [x] Complete restart, installed bytes, immutable cache, cancellation, mixed technical failure and retained-output fallback coverage.
- [x] Finish independent review; run full race, vet and tagged E2E plus diff check. Record exact results and compatibility behavior in the implementation ledger and current guides.
