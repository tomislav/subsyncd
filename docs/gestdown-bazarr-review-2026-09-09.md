# Gestdown comparison with Bazarr — 2026-09-09

Reviewed subsyncd `a141139`, Bazarr's current [Gestdown provider](https://github.com/morpheus65535/bazarr/blob/master/custom_libs/subliminal_patch/providers/gestdown.py), and Gestdown's current official source/API. The review below records the original findings; all four were subsequently repaired after user authorization.

## Findings

### P2 — Catalog-entry subtitle IDs are silently dropped

`internal/provider/gestdown/client.go:71` accepts regular UUIDs and `sp_{uuid}_ep_{N}` only. The current upstream [SubtitleDto](https://github.com/Belphemur/AddictedProxy/blob/main/AddictedProxy/Model/Dto/SubtitleDto.cs) and [download/search controller](https://github.com/Belphemur/AddictedProxy/blob/main/AddictedProxy/Controllers/Rest/SubtitlesController.cs) also produce and consume `sp_{packUuid}_entry_{entryUuid}` for individual cataloged files. The search normalization guard drops these otherwise valid completed episode candidates. The later `_ep_` suffix check would also need adjustment; widening the regex alone is insufficient.

A local fake-server reproduction returned a valid entry ID and observed zero candidates. A read-only public download lookup using all-zero nonexistent pack/entry UUIDs returned HTTP 404 with a pack-not-found response, confirming the deployed service recognizes the form. No real subtitle was downloaded by that probe. The current live OpenAPI still reports version 5.2.0 and omits the form from its description. Bazarr keeps IDs opaque and follows the supplied download reference, so it does not impose our format exclusion.

Recommendation: explicitly support the entry grammar, retain fixed-origin reconstructed downloads and response episode checks, and test regular and HI entries. Continue rejecting whole-pack IDs in the episode adapter.

### P2 — Comma-separated versions lose matching evidence

`internal/provider/gestdown/client.go:154` stores `version` as one release string. The shared parser only splits spaced slash alternatives. Bazarr treats commas as version separators.

A local scoring reproduction for target group `GROUPB`/source `bluray` scored `WEB-DL-GROUPA, BluRay-GROUPB` at 65 but the reversed order at 40. With the same alternatives represented separately, group and source should both remain available. This affects release ranking and the three-candidate shortlist.

Recommendation: split Gestdown version alternatives at its adapter boundary. Preserve the full `release` field separately, without changing comma handling for other providers' titles.

### P2 — Bare and informal release groups lose 25 points

Gestdown versions can be group labels rather than full torrent filenames. `match.ParseRelease` leaves the group empty for `0tv`, `DVDRip ORPHEUS`, and `BluRayREWARD`; all appeared in the real metadata obtained during the earlier live contract check. Bazarr has a group-text fallback. Our local scoring probes awarded zero release-group points even when the media's group was respectively `0tv`, `ORPHEUS`, or `REWARD`.

Recommendation: preserve provider version evidence and recognize group labels conservatively, including existing group equivalence rules. Do not copy Bazarr's arbitrary substring matching or manufacture title/year/episode identity. Test both matching and deceptive near-match labels.

### P2 — Explicit quality alternatives are discarded

`internal/provider/gestdown/client.go:86` does not decode `qualities`. The upstream DTO defines it as available video qualities. This is stronger evidence than the generic `hd` boolean, and can represent several compatible resolutions without selecting an invented single value.

A fake API response with `qualities: ["720p", "1080p"]`, version `Bluray-CtrlHD`, and no full release produced zero resolution points for a 1080p target. Bazarr checks membership in the list. Our omission loses five points and can affect shortlist ordering/thresholds.

Recommendation: retain explicit supported-resolution evidence and award the existing resolution contribution on membership. Keep absent/unknown values neutral and avoid inventing a resolution from `hd`.

## Differences that do not justify copying Bazarr

- subsyncd persists cooldowns instead of sleeping inside provider calls; this is required by worker lease semantics.
- Returned identity/language checks and redirect restrictions implement existing project boundaries. The previously verified real download does not require redirects.
- Bazarr advertises Serbian script variants but passes only the base alpha-3 language to its converter. That does not prove script-specific response evidence. Keep our explicit variant rejection until supported end to end.
- The episode lookup and download flow itself worked in the previous real contract test. This review exposes candidate-coverage and scoring gaps that one successful download cannot exercise.

## Verification

Temporary Go test overlays under `/tmp` exercised the actual matcher and adapter with synthetic local responses. Race-enabled probes confirmed all four findings. No production installation was accessed. The only live API calls in this review fetched the public schema and looked up a nonexistent entry ID. No runtime tests or implementation files were changed.

## Repair status

All four findings are fixed in the `fix: preserve Gestdown catalog and matching evidence` change based on `a141139`. Catalog-entry IDs pass validated fixed-origin downloads; comma alternatives split in the adapter; bounded version labels and explicit supported qualities populate optional group/resolution evidence. Scoring weights and identity/LAPSE policies remain unchanged.

Regression tests reproduced each original failure before repair. Duplicate merging, cache round trips, provider-specific cache refresh, rejection-signature sets and LAPSE policy gates are covered. Old Gestdown search results refresh under `gestdown-evidence-v2`, without invalidating other providers. Full race/e2e/vet and real download verification are recorded in the implementation ledger.
