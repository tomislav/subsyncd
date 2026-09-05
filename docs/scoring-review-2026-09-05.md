# Scoring review — 2026-09-05

Scope: matching, provider normalization, catalog scoring metadata, and workflow eligibility/bypass consumers, based on `b9e5854` plus the completed local scoring repairs. This is a focused code review, not a live-provider contract validation or a translation-quality assessment.

## Initial source fixes

- Explicit whitespace-delimited slash alternatives now retain each listed source for matching. The sanitized movie regression changes from 39 to 54 when a Blu-ray alternative follows or precedes WEB-DL. The source bonus remains capped at 15; compact slashes and remux distinctions are preserved.
- Target source aliases are normalized with the existing source normalizer. A `webdl` target previously earned zero source points against `WEB-DL`; it now earns 15.
- Both defects were reproduced with failing tests before implementation. The follow-up repairs below extend alternative parsing, tighten SubDL identity provenance, preserve duplicate evidence, and enrich catalog metadata without changing scoring weights or bypass policy.

## Findings repaired locally

All four findings below were subsequently authorized for repair. Their descriptions record the pre-fix behavior; the resolution paragraphs describe the final implementation.

### P1 — SubDL request defaults become false identity evidence

`internal/provider/subdl/client.go`, `normalize`, fills absent response title/year from the requested media. `match.Evaluate` then awards these values 15 title/year points; `canBypassLapse` treats them as an identity anchor.

A retained adapter-to-workflow regression supplies a non-pack episode result with no response identity and release `S01E02.WEB-DL.1080p-GROUP`. For an `Example Show` target with year 2020 and matching episode/group/source/resolution, normalization copied the target title/year and scoring returned 80 (15 + 20 + 25 + 15 + 5). That satisfies the default bypass threshold and identity/group/episode predicates without independent title or external-ID evidence. No live response or installation was involved.

Resolution: absent response title/year now remain unknown. Adapter scoring tests retain genuine returned or parsed title/year evidence. A fake-server adapter-to-workflow test verifies 65 points and no bypass for missing identity, including with a lowered threshold, versus 80 and bypass for returned identity. The regression was also checked against original source using a temporary overlay. Normalized search caches advance to `candidate-v3`.

### P2 — Slash alternatives still lose group and resolution evidence

`internal/match/score.go`, `Evaluate`, still calls `ParseRelease` once per raw descriptor for evidence other than source. A local probe used `Example.Movie.2020.720p.WEB-DL-GROUPA / Example.Movie.2020.2160p.BluRay-GROUPB`, targeting GROUPB/2160p. The parser retained GROUPB but 720p: 25 group/resolution points. Reversing the alternatives retained GROUPA and 2160p: only 5 points. The candidate advertises the same releases in both orders.

Resolution: a shared alternative parser now feeds scoring, `HasEpisodeEvidence`, and `HasMatchingEdition`. Group/source/resolution are stable across alternative order; edition conflict/unknown controls and explicit episode-conflict tests preserve the existing policy. Combined descriptors are replaced by their individual alternatives rather than retained as extra evidence.

### P2 — SubDL drops duplicate evidence before the shared merger

`internal/provider/subdl/client.go`, `appendUniqueItems`, discards later rows with an already-seen URL/name. This occurs across standard, absolute-episode, and season search responses before normalized candidates reach shared deduplication.

A local probe passed the same sanitized `/example.zip` identity twice: first with WEB-DL, rating 0 and download count 0; later with Blu-ray, rating 1 and count 10000. Only the first row survived, losing the later release evidence and quality signals. This violates the documented strongest-quality/stable-release-union contract and can lower the score or exclude the candidate from the shortlist.

Resolution: all raw fallback rows reach normalization, then use the shared stable-identity merger. Incomplete episode rows are filtered after merging so later rows can fill missing coordinates; explicit wrong coordinates remain excluded. Regressions cover later scoring evidence, earlier incomplete identity, unknown-only and wrong-episode controls; existing direct-member/pack and credential-safe tests pass.

### P2 — Catalog hydration does not supply streaming-service evidence

The Sonarr and Radarr media constructors populate release name, group, source and resolution but omit `Media.StreamingService`. No catalog/application enrichment assigns it. The scorer requires a nonempty target streaming service, so normal catalog imports cannot earn the documented 5 points even when both release names contain an explicit service marker.

Resolution: both catalogs derive the service from explicit web-release scene-name metadata after a year/episode marker. Fake-server hydration tests cover NF and AMZN. Negative controls cover movie/show title words, missing markers, non-web sources and empty metadata. Existing rows receive this metadata on normal hydration; startup behavior is unchanged.

## Policy observations

Release score is primary, the broad shortlist is capped at three, and LAPSE confidence breaks ties only within an equal-score tier. Thus a small rating/popularity advantage can keep a lower-scored but better-timed result out of consideration. This follows the current documented policy; changing it is a design decision, not a correction to the score arithmetic. Translation quality is not measured by this score or by LAPSE.

Review probes were temporary, local-only diagnostic tests and were removed after their outputs were recorded here. Retained regression tests cover all repaired findings and boundaries. Independent review reported no remaining material findings after the incomplete-episode merge fix. Full race tests, vet, tagged E2E and Compose validation passed with local fixtures only. The user authorized committing and pushing the verified repair to GitHub; production deployment remains separate. Scoring weights and shortlist policy are unchanged pending evidence from corrected results.
