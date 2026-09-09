# OpenSubtitles, SubDL and Titlovi comparison with Bazarr

Reviewed subsyncd commit `9310c158628d62e2fb2e10f936442d81cc798414` against the requested Bazarr master adapters on 2026-09-09. This is a review, not an implementation change. The earlier repaired findings in `provider-review-2026-09-05.md` are not repeated.

Follow-up: the user authorized all seven repairs and a push. The subsequent `fix: repair provider discovery and subtitle evidence` change implements R1–R7 with durable regressions. Independent review additionally tightened annotation negation and legacy SubDL pack-cache compatibility. The sections below retain the original review evidence; current behavior is recorded in `development/providers.md` and `implementation-status.md`. Live frame-based OpenSubtitles conversion remains unverified; local fixtures verify the request and extraction contract.

## Findings

### R1 — P1: SubDL loses forced and hearing-impaired annotations

`internal/provider/subdl/client.go:122` does not request `comment=1`, the response model at line 165 has no comment field, and normalization at line 254 never sets `Forced`. HI recognition relies solely on the structured `hi` value. Explicit comments such as “Forced subtitles for foreign dialogue” or “SDH subtitles”, and an archive name such as `Example_HI_English.zip`, lose their policy evidence.

Synthetic cases normalize with both flags false. If extracted filenames are ordinary, the downstream forced-filename selector cannot recover comment-only evidence; there is no corresponding archive HI policy check. Such candidates can remain eligible under a policy that excludes them. Timing validation does not establish subtitle completeness.

[Bazarr requests and interprets these annotations](https://github.com/morpheus65535/bazarr/blob/master/custom_libs/subliminal_patch/providers/subdl.py#L375). The [SubDL API documentation](https://subdl.com/api-doc) confirms that comments must explicitly be requested. Repair request, decoding and conservative annotation recognition together, with negative/ambiguous wording tests rather than copying unrestricted substring heuristics.

### R2 — P2: OpenSubtitles ID searches carry conflicting filters

`internal/provider/opensubtitles/client.go:160` always adds title and year after choosing an external ID. Sonarr media year is the series premiere year (`internal/catalog/sonarr.go:252`), whereas the subtitle endpoint's year parameter filters movie/episode year. Later-season searches can exclude valid episodes; an alternate or translated title can also unnecessarily constrain an ID lookup.

The fixture produces `parent_imdb_id=1234567`, season 3, episode 2, plus `query=Example Show` and `year=2024`. [Bazarr uses the parent ID and coordinates without these extra filters](https://github.com/morpheus65535/bazarr/blob/master/custom_libs/subliminal_patch/providers/opensubtitlescom.py#L285). The [official schema](https://stoplight.io/api/v1/projects/opensubtitles/opensubtitles-api/nodes/open_api.json) recommends ID searches instead of text and warns that conflicting filters reduce results. Avoid treating a series year as an episode year, including title-only searches.

### R3 — P2: OpenSubtitles does not request a supported download format

`internal/provider/opensubtitles/client.go:445` sends only `file_id`. A returned MicroDVD `.sub` downloads but fails the SRT/ASS/SSA/VTT parser in `internal/pack/archive.go:364`. [Bazarr requests SRT](https://github.com/morpheus65535/bazarr/blob/master/custom_libs/subliminal_patch/providers/opensubtitlescom.py#L419), using the API's [format conversion parameter](https://opensubtitles.stoplight.io/docs/opensubtitles-api/6be7f6ae2d918-download).

The injected transport confirms the missing parameter and extraction failure. It does not establish the current remote default or prove successful conversion of every format. A repair should request a supported format and verify the documented FPS requirements for frame-based conversion. Existing deterministic rejection/cache behavior must be considered when making previously unusable results downloadable.

### R4 — P2: SubDL movie searches lack the alternate-ID fallback

`internal/provider/subdl/client.go:142` prefers IMDb and never tries an available TMDB ID after an empty movie lookup; lines 99–113 limit fallback to episodes. The fixture returns a recognized no-film response for IMDb and a subtitle for TMDB. The adapter makes only the IMDb call and returns zero candidates successfully.

[Bazarr retries the alternate movie ID](https://github.com/morpheus65535/bazarr/blob/master/custom_libs/subliminal_patch/providers/subdl.py#L230). Add a bounded retry only after genuine no-results, preserving authentication, quota, cancellation and technical-failure handling. Continue deriving scoring identity from returned data.

### R5 — P2: Titlovi drops its returned AKA title

`internal/provider/titlovi/client.go:259` splits `Original Name AKA English Movie` and discards the second title. If the target title is the second half and neither Arr alternatives nor release names supply the match, the candidate loses genuine identity evidence and 15 title/year points. The synthetic movie scores 20 instead of 35, falling below the default eligibility threshold.

[Bazarr retains and matches both titles](https://github.com/morpheus65535/bazarr/blob/master/custom_libs/subliminal_patch/providers/titlovi.py#L244). Preserve returned alternate-title evidence without substituting query identity. Any candidate evidence extension must survive cache serialization, duplicate merging and stable rejection signatures.

### R6 — P2: Titlovi rewrites an allowed absolute download origin

`internal/provider/titlovi/client.go:277` strips the origin from accepted absolute links. Download reconstructs them against the configured base at line 308. A returned `https://kodi.titlovi.com/download/123` therefore requests `https://titlovi.com/download/123`. Both hosts are allowed by default, but need not serve the same resource paths.

[Bazarr downloads the returned link](https://github.com/morpheus65535/bazarr/blob/master/custom_libs/subliminal_patch/providers/titlovi.py#L294). Preserve an accepted origin while retaining host validation and credential-free persistence. The fixture verifies the rewrite; current live prevalence of alternate-origin links remains unverified.

### R7 — P2: OpenSubtitles language aliases depend on response casing

`internal/provider/opensubtitles/client.go:190` recognizes `pt-PT` and `zh-CN` before canonicalization, but not lowercase equivalents. The official schema's language examples include `pt-pt` and `zh-cn`; these normalize to regional tags that then fail equality with configured `pt` and `zh`.

This is an additional API-contract robustness finding, not an advantage demonstrated by Bazarr, whose reverse aliases are also case-sensitive. Canonicalize before applying established provider aliases. Do not infer new script/region equivalences such as `zh-Hant` or `es-419` from this issue.

## Differences to retain and unresolved observations

- Preserve separate exact/broad phases, returned identity evidence, all OpenSubtitles files and pagination, strict episode/pack selection, bounded extraction, persisted cooldowns, validated hosts and credential-free signed downloads. Bazarr's permissive member selection and sleeps are not suitable replacements.
- Previously repaired Titlovi refresh pagination, fabricated IMDb evidence, SubDL range/absolute/direct-member behavior and OpenSubtitles login/quota/credential handling remain covered; no regression was found in this comparison.
- Titlovi sends year/episode filters that Bazarr omits. Provider semantics were not established sufficiently to call this another bug. Serbian script bundles and extra naming aliases also need targeted contract evidence before changing selection policy.
- Existing OpenSubtitles pagination-bound and returned-translation-flag hardening observations remain in the earlier report; they are not new parity findings.

## Verification and limits

The existing provider and matcher suites passed:

```sh
GOCACHE=/tmp/subsyncd-fix-cache GOMODCACHE=/tmp/subsyncd-fix-mod go test ./internal/provider/... ./internal/match -race -count=1
```

Independent provider reviews and controller reruns reproduced the new gaps through temporary Go overlays. Expected-correct assertions intentionally fail on the reviewed implementation:

```sh
GOCACHE=/tmp/subsyncd-fix-cache GOMODCACHE=/tmp/subsyncd-fix-mod go test -overlay=/tmp/subdl-bazarr-review/overlay.json ./internal/provider/subdl -run TestReviewSubDL -race -count=1 -v
GOCACHE=/tmp/subsyncd-fix-cache GOMODCACHE=/tmp/subsyncd-fix-mod go test -overlay=/tmp/subsyncd-titlovi-bazarr-review/overlay.json ./internal/provider/titlovi -run TestBazarrReview -race -count=1 -v
GOCACHE=/tmp/subsyncd-fix-cache GOMODCACHE=/tmp/subsyncd-fix-mod go test -overlay=/tmp/os-bazarr-review/overlay.json ./internal/provider/opensubtitles -run TestBazarrReview -race -count=1 -v
```

These temporary overlays are session artifacts, not committed tests. They use sanitized fixtures and injected HTTP transports; no provider accounts, live subtitle downloads, Arr services or production installations were accessed. Only this report and the handoff ledger were changed. No fixes, commit or push were performed. Next task: implement the selected findings with durable regressions if requested, including appropriate cache/rejection evidence versioning.
