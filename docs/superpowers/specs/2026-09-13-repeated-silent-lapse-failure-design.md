# Repeated Silent LAPSE Failure Quarantine Design

## Context

LAPSE v2.0.5 can terminate with exit code 2 while producing neither JSON on
stdout nor a diagnostic on stderr for a particular otherwise-valid subtitle
artifact. The production case was reproducible with the same media and cached
Titlovi member, while the same LAPSE binary and media returned a normal verdict
for another subtitle. Today this remains a technical failure forever, so the
same immutable candidate can repeatedly consume workflow attempts without ever
allowing the search to move past it permanently.

This design introduces a narrow, evidence-based exception to the rule that
process failures never poison candidate rejection state. All ordinary process,
protocol, timeout, cancellation, filesystem, and diagnostic-bearing failures
remain technical.

## Goals

- Keep the first silent exit-2 failure retryable as a technical failure.
- Quarantine only an identical candidate artifact after the same silent exit-2
  failure is observed on a second workflow attempt. The same immutable identity
  is invoked and counted at most once within any one attempt.
- Continue the current acquisition after confirmation so other candidates and
  providers remain eligible.
- Survive daemon and host restarts without broadening the rejection identity.
- Reconsider the candidate when any material media, candidate, LAPSE, or policy
  input changes, or when the operator explicitly retries rejected candidates.

## Non-goals

- Interpreting every LAPSE exit code 2 as deterministic.
- Quarantining timeouts, cancellations, runner failures, malformed JSON with
  nonempty output, filesystem failures, or failures with any stderr diagnostic.
- Retrying LAPSE twice within one workflow invocation.
- Changing LAPSE scoring, shortlist ordering, provider cooldowns, missing-result
  scheduling, or the one-output-producing-invocation-per-candidate contract.

## Typed failure boundary

The LAPSE adapter will return a sanitized typed process-exit error only when the
process completed with a nonzero exit and its output could not be decoded as the
LAPSE JSON protocol. The type exposes only the numeric exit code and booleans
stating whether stdout and stderr were empty. It never exposes either buffer,
paths, command arguments, or raw runner errors.

A failure is strike-eligible only when all of these are true:

- the typed error reports exit code 2;
- stdout is empty;
- stderr is empty;
- the caller was not canceled and the invocation did not time out; and
- the selected artifact checksum is available.

Normal structured `unsure` and `nothing` reports retain their existing direct
candidate-rejection behavior. A code-2 process exit with any output remains a
technical failure and is not counted.

## Durable strike identity and storage

Migration `013_repeated_lapse_failures.sql` adds
`candidate_lapse_failures`. Each row is owned by `media` through a cascading
foreign key and is unique over the same stable identity used to decide whether
a candidate rejection applies:

- media ID and canonical language;
- provider ID and result ID;
- stable candidate signature;
- selected artifact checksum;
- LAPSE/tool signature, including compatibility version and synchronization
  policy;
- media path, Arr file ID, size, and nanosecond modification time; and
- sanitized failure signature (`exit_2_empty_output`).

The row stores a bounded occurrence count and first/last observation times. An
upsert for an identical identity atomically increments the count up to two and
returns the resulting count. Different media bytes, paths, Arr file IDs,
candidate evidence, artifact bytes, LAPSE versions, or policies cannot inherit
the strike.

## Workflow behavior

When candidate preparation receives a strike-eligible failure, it records the
occurrence before classifying the outcome:

1. Count 1 returns the original technical failure. The worker advances only the
   technical-failure schedule.
2. Count 2 atomically persists the normal candidate rejection with reason
   `lapse_repeated_empty_exit`, removes the matching strike row, emits the
   bounded candidate-rejection event, and treats that artifact as a
   deterministic candidate-local rejection.
3. The workflow continues through remaining versions, candidates, score tiers,
   and provider tiers under their existing rules. If no candidate installs, the
   existing deterministic/mixed outcome classifier decides the job result.

Persisting the second strike and rejection must be one SQLite transaction. A
repository failure remains technical and must not pretend that the candidate
was quarantined. The workflow performs no immediate confirmation invocation;
the two observations must come from separate workflow attempts. A request-local
identity fence prevents pack cache, exact, broad, preferred, or fallback
acquisition from invoking LAPSE or recording another strike for the same
candidate/artifact/rejection identity within one attempt.

Any composite failure containing a process-exit error remains technical as a
whole, including when another cause is a verdict, archive selection, content,
no-speech, cancellation, filesystem, or protocol error. It must be classified
before the established deterministic candidate-rejection cases.

Successful synchronization and other deterministic outcomes may delete a
matching stale strike best-effort only as part of an explicit repository
operation whose failure remains technical. Rows for identities that stop being
encountered are harmless and cascade when their media is removed.

## Manual retry and observability

`search --retry-rejected` clears both candidate rejections and LAPSE strike rows
for the selected media/language. Existing reset paths that clear candidate
rejections adopt the same behavior. Ordinary searches never clear strikes.

`explain` continues to list confirmed candidate rejections. It does not expose
unconfirmed strikes, avoiding a new operator-facing state model for a single
technical retry. Structured logs add no raw LAPSE output or paths. The existing
failure event remains unchanged on strike 1; confirmation uses the existing
candidate-rejection event with the new bounded reason code.

## Compatibility and migration

The migration is additive and does not reinterpret historical process errors.
Existing searches, attempts, provider-resume checkpoints, candidate
rejections, caches, installations, and reconciliation state remain unchanged.
The first qualifying failure after deployment becomes strike 1 even if logs
show earlier occurrences.

This design deliberately tightens the existing invariant as follows: process
failures never create candidate rejection state **except** after two durable,
identical observations of LAPSE exit 2 with completely empty stdout and stderr.
All other process failures remain technical.

## Tests and verification

Focused red/green tests will cover:

- sanitized typed LAPSE errors for empty and nonempty output;
- no strike for timeout, cancellation, runner, protocol, diagnostic-bearing, or
  missing-checksum failures;
- strike 1 surviving database reopen and remaining technical;
- strike 2 atomically creating `lapse_repeated_empty_exit` and removing the
  strike;
- fingerprint changes preventing strike inheritance;
- second-strike persistence failure remaining technical;
- confirmed rejection advancing to another candidate/provider;
- pack-cache/provider duplication producing only one invocation and strike per
  workflow attempt;
- process-bearing composite failures remaining technical before deterministic
  classification;
- manual rejected-candidate retry clearing both durable states; and
- migration preservation of all pre-existing durable tables.

Final verification runs the affected packages with `-race`, the full repository
race suite, `go vet ./...`, tagged E2E tests, `gofmt -l`, and `git diff --check`.
All provider and Arr interactions use local fakes and sanitized fixtures.
