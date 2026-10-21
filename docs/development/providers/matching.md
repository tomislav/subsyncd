# Matching

How one media/language job searches providers, gates and scores candidates, and selects subtitles from archives. Terms are defined in the [glossary](README.md#glossary).

- [Search order](#search-order)
- [Caches](#caches)
- [Identity gates](#identity-gates)
- [Score model](#score-model)
- [Release names and sources](#release-names-and-sources)
- [Editions](#editions)
- [Ranking and upgrades](#ranking-and-upgrades)
- [Language check](#language-check)
- [Subtitle normalization](#subtitle-normalization)
- [Archive and pack selection](#archive-and-pack-selection)
- [Episode tokens](#episode-tokens)
- [Episode ranges](#episode-ranges)

## Search order

Provider tier comes before cache, exact and broad preference: the sequence below runs within the preferred tier first, and in the fallback tier only after acquisition is exhausted. Each tier has its own three-result broad shortlist. Inventory, repository, cancellation, media, publication and rollback failures are terminal and never open another tier.

1. **Inventory.** Reuse embedded tracks only when a completed probe matches path, Arr file ID, size and mtime (empty probes are valid; catalog rows alone are not). Always rescan sibling sidecars. A stale or deleted catalog identity aborts technically.
2. **Satisfied?** Stop for a full matching embedded track or a protected sidecar. Forced-only and unknown-language tracks do not satisfy a normal request.
3. **Pack cache.** Try a reusable, checksummed season-pack member.
4. **Exact phase.** Ask hash-capable providers one after another in configured order. Exact candidates are uncapped and downloaded one at a time; a forced, rejected, malformed or wrong-member candidate advances to the next. Stop at the first committed install.
5. **Broad phase.** Ask every assigned provider and merge results in configured order. Duplicate `(provider_id, result_id)` rows collapse before caching, persistence, scoring or shortlisting; later duplicates fill missing identity fields, and conflicting values keep the first. When the episode's release name gives another season (see [Release-numbered search](#release-numbered-search)), the broad phase runs again in that numbering and adds its results.
6. **Persist evidence** (score and rejection) for every result, never a signed download URL or token.
7. **Drop rejected candidates** before shortlisting, so later-ranked candidates advance.
8. **Shortlist** the best three eligible broad candidates and split them into score tiers.
9. **Prepare** only the highest remaining tier, lazily. See [LAPSE](lapse.md) for bypasses and the tournament.

Download availability is checked before each phase and each download; see [Limits](limits.md#preflight-and-the-final-gate).

Candidate downloads, extraction and LAPSE scratch use the system temporary directory. Raw archives and discarded candidates are released promptly; viable tied candidates stay available as installation fallbacks.

### Release-numbered search

Sonarr may number a show as one long season that releases and providers split in two: the file Sonarr calls S01E10 is released as `Show.S02E02…`, and providers file its subtitles under season 2. A search in Sonarr's numbering only finds season 1 packs. One Hundred Years of Solitude is numbered S01E01–E16 in Sonarr, while releases and Titlovi use S01 and S02 of 8.

- **When.** `pack.ReleaseEpisode` gives a season different from Arr's: the release name holds exactly one complete episode token, no range, in another season. Episode ranges never qualify.
- **What.** A second broad search (`search.release_numbered`) with the media under the release's numbering: season and episode replaced, absolute numbers cleared, file, identity and titles unchanged. It runs in each provider tier, with the same resume skips.
- **Merging.** Its results are tagged `ReleaseNumbered` and appended; a result both searches returned keeps its Arr identity. A provider that found something under the release's numbering is not counted as empty. Its provider errors are only logged: throttling, resume and outcomes follow the search in Arr's numbering.
- **Scoring and selection.** A tagged result is scored by the identity gates and score model, and its archive members selected, against the release's numbering, so the Part 2 pack's `S02E02` member matches through the ordinary `episode_token` rule.
- **Safety.** A tagged result never takes the score bypass, so LAPSE always checks it against the file; under `sync.policy: never` it is rejected as `pack_selection` (rule `release_numbered`) instead. Its packs are never published to the pack cache, which files packs under their season. Installation, rejection identity and the destination path stay Arr's.
- **Cost.** One more broad search per provider for such an episode. Exact-hash searches are not repeated: a hash identifies the file, not its numbering.

## Caches

| Cache | Key | Lifetime |
| --- | --- | --- |
| Search results (`provider_cache`) | Provider, phase, language, file fingerprint, release name, and every identity searched by: kind, title, year, IMDb/TMDB/TVDB IDs, season, episode and range, absolute numbering, and the adapter's cache version | 6 hours, including empty results |
| Season packs (`pack_cache`, `pack-cache/`) | Content address | `pack_cache.ttl` (24 hours) within a total `pack_cache.max_bytes` (512 MiB) LRU ceiling |
| SubSource titles and listings | In memory | 6 hours, lost on restart |
| SubDL search answers | In memory | 6 hours, lost on restart |

- A corrected Arr identity is searched afresh rather than served results for the old one.
- Cache selection filters provider membership before accepting or touching an entry.
- Versioned records mark any changed result identity, download reference or direct-member reference as needing a fresh search before acquisition. A refresh failure is a technical error; it never falls back to stale links. Titlovi query-bearing links therefore always need a refresh.
- Empty, root-only, userinfo-bearing and network-path references are rejected before download.
- Packs are immutable. Every cache hit reloads the manifest, verifies checksums, and reruns strict member selection for the current episode. Lookup skips rejected members before touching the entry's access time.

## Identity gates

Known identity conflicts reject before points are counted:

- language, media kind, and forced-only results for a full-language request;
- external IDs;
- movie year beyond ±1 without a matching external ID;
- season or episode outside the pack's scope;
- a different known cut (see [Editions](#editions));
- a candidate naming one disc of a multi-CD release (`CD1`, `(CD 2)`, `-CD1`, `2cd`, `2CDs` as whole tokens in any release name), because it covers only part of the film.

A verified exact file hash overrides only a conflicting textual edition or disc label; every other gate applies. Unknown evidence is normally neutral.

## Score model

| Signal | Points |
| --- | ---: |
| Verified exact media-file hash | 100 (terminal within its provider tier) |
| Matching IMDb/TMDB/TVDB ID | 20 |
| Normalized title and year | 15 |
| Matching episode, parsed range containing it, or containing pack | 20 |
| Release group | 25 |
| Source | 15 |
| Edition/cut | 10 |
| Streaming service | 5 |
| Resolution | 5 |
| Provider rating | 0–3 |
| Popularity/downloads | 0–2 |

- Non-hash totals are capped at 100 and must reach `minimum_release_score` (default 35, range 1–100) to be eligible.
- Each signal is awarded at most once.
- Rating points are `round(rating × 3)` of the provider's normalized, vote-weighted rating (see [OpenSubtitles](adapters.md#search)).
- Popularity is `min(log10(downloads + 1) / 4, 1)` and earns `round(popularity × 2)` points: the second point from about 1,000 downloads, saturating at about 10,000.
- Rating and popularity never replace the anchors a LAPSE bypass needs (see [LAPSE](lapse.md#synchronization-policy)).

## Release names and sources

- Provider descriptors listing alternatives separated by a slash with whitespace on both sides (tabs and Unicode spaces included) are parsed as separate release names, so alternative order cannot hide a matching source, group, resolution or edition. Compact slashes stay part of the descriptor.
- Source names map to release terms on both sides, in scoring and in `ReleaseAffinity`:

  | Values | Source |
  | --- | --- |
  | `television`, `televisionRaw`, `tv`, `PDTV` | `hdtv` |
  | `web`, `webdl`, `WEB-DL` | `web-dl` |
  | `dvd` | `dvdrip` |
  | `BRRip` | `bluray` |
  | `blurayRaw`, `BDRemux`, `BR-DISK`, `BDMV` | `remux` |

- Radarr reports a remux as source `bluray`, so a media quality naming `Remux` or `BR-DISK` makes the media source `remux` (`mediaSource`). Blu-ray remux stays distinct from Blu-ray encodes.
- Aliases apply only to a whole source value; `TV` in a release title names no source.
- A specific source token in the release name (`remux`, `WEB-DL`, `WEBRip`, `BluRay`, `HDTV`) wins over the parser's whole value, so `Show.1080p.WEB.h264-GRP.WEBRip` is `webrip`.
- `COMPLETE.BLURAY` is not a full disc, since `COMPLETE` also marks a complete season.
- Sonarr and Radarr hydration derives streaming-service evidence only from explicit web-release scene names after a year or season/episode marker. Service names in titles never count.

## Editions

- Only the cut can conflict: Director's Cut, Final Cut, Ultimate Cut, Ultimate Edition, Special Edition, Anniversary Edition, Redux, Extended, Unrated, Theatrical, and any other edition that calls itself a cut (`Assembly Cut`). A known different cut is rejected.
- Every other label (Hybrid, Dual Audio, Dubbed, Multi Subs, B&W, IMAX Enhanced, Remastered, Criterion…) names no cut and is neutral, so `Ultimate Edition Remastered` compares as `ultimate edition`.
- Picture variants (IMAX, remastered, restored, open matte, hybrid) are tracked separately. When the file names no cut, a candidate carrying all of the file's variant labels earns the 10 edition points; variants never conflict.
- Edition markers are read from the release descriptor after the movie year when there is one, so a title with edition-like words is not mistaken for an edition. Punctuation is normalized before comparison.

## Ranking and upgrades

- Release score is primary; a lower score tier cannot outrank a solid higher tier.
- **Broad shortlist** ties: provider priority, rating, raw download count (which keeps discriminating after popularity saturates), provider ID, result ID.
- **Within a prepared score tier**: LAPSE confidence, then provider priority, rating, provider ID, result ID.
- A managed subtitle is upgraded within its tier only by an exact hash or a score at least 10 higher (`DefaultMinimumUpgradeDelta`). Non-exact upgrades use LAPSE by default.
- Fallback-to-preferred promotion ignores the delta and the existing-exact rule but keeps eligibility and LAPSE gates. Preferred-to-fallback downgrades never happen.
- A candidate-local installer validation failure advances to the next prepared tie or score tier, recording the original artifact's rejection evidence. A format-changing upgrade advances without quarantining the candidate.

## Language check

After download, `prepareCandidate` runs `langcheck.Check` for Croatian and Bosnian requests. It compares whole words (split on Unicode letters) against fixed lists:

| Result | Rule |
| --- | --- |
| `not_south_slavic` | At least 200 words, and South Slavic function words (`je`, `da`, `se`, `ne`, `nije`, `što`/`šta`, …, excluding words as common in English such as `i`, `to`, `a`) make up less than 12%. Croatian subtitles measured at least 21%, a Portuguese one 2.8% |
| `serbian_ekavian` | At least five exact ekavian forms (`gde`, `ovde`, `uvek`, `vreme`, `lepo`, `deca`, `mesto`, …; never prefixes, since Croatian `vrijeme` inflects to `vremena`) and at most a tenth as many ijekavian ones |

Either rejects the candidate as `language_mismatch` before LAPSE or any bypass. Exact-hash candidates are checked too, because a hash matches the media, not the language. Mixed texts pass.

## Subtitle normalization

Every downloaded subtitle file is normalized before selection (`normalizeSubtitle` and `planRepairs` in `internal/pack`). `subtitleorder.Repair` rewrites the raw records, so styling, positions, cue settings and ASS fields survive. These are deterministic content rules: a file they reject is remembered as `invalid_subtitle`.

### Decoding and conversion

- Text is decoded as UTF-8, UTF-16 with a byte order mark, or Windows-1250.
- A MicroDVD `.sub` (first line `{frame}{frame}`) is converted to SRT when it states its frame rate in a `{1}{1}25.000` or `{0}{0}` header; a decimal comma (`23,976`) and a trailing Ctrl-Z are accepted. A leading `/` or lowercase `{y:i}` italicizes its line, an uppercase `{Y:i}` the whole cue.
- An unconvertible `.sub` (such as binary VobSub) is ignored like any extra file and not counted as skipped, so `single_generic` still selects the real subtitle beside it. An extensionless download is checked for MicroDVD by content. A converted `Movie.sub` is named `Movie.sub.srt`.

### Cue repairs

- Cues with no text are dropped. In SRT and WebVTT, exact repeats (same start, end and text) are dropped too; ASS/SSA layers legitimately repeat lines.
- A cue whose end comes before its start ends after its reading time (one second plus 50 ms per character, at most seven seconds) or at the next cue, whichever is sooner.
- **Mistyped starts** (SRT and WebVTT): a cue that starts more than 10 s before the furthest start so far while its end continues from there (within 10 s) moves to the end of the cue holding that furthest start. A cue whose end is early too belongs to a misplaced block, and one the move would leave shorter than its reading time is a long caption; both are only sorted. ASS/SSA starts are never retimed.
- **Mistyped ends** (SRT and WebVTT): a cue that ends more than 10 s after the next cue starts, lasts over three times its reading time and over 15 s, ends at the next cue's start (or at the end of its reading time, when later).
- Cues set apart from dialogue are never retimed (`heldApart`): lyrics (`♪` or `♫` anywhere, `#` at the start or end of a line), cues whose every line is italic, and positioned cues (`{\an8}`, `{\pos(…)}`, WebVTT `line`/`position`).
- All cues are then sorted by start time, and unclosed `<i>` tags in SRT and WebVTT are closed.

### Record matching

`subtitleorder` finds SRT and WebVTT records the way astisub does:

- every line containing `-->` is a timing line, with or without spaces around the arrow;
- an optional index precedes it: in SRT the non-blank line just before it (`1` or `1.`), in WebVTT a line after a blank line;
- an SRT index without a blank line before it is accepted when it is a number following a line of text, even when numbering skips or restarts. A non-number (`Second` in a file without indexes or blank lines), or a line directly after a timing line, is text; rewriting would lose it, so the records are `ErrUnsafe`;
- a last text line that is a number cannot be told from an index and is lost, as astisub loses it;
- an SRT record runs to the next index or timing line, so a missing blank line or a blank line inside a cue keeps text with its cue. A WebVTT cue ends at a blank line.

When records cannot be matched to the parsed cues (`ErrUnsafe`), the decoded text is kept unchanged if every cue already starts in order and none needs retiming (`onlyTidies`); otherwise the file is rejected, since installation would reject such cues anyway.

### Rejections

- A negative timing (`-00:00:05,000`, which astisub reads as positive) is rejected.
- A file whose cues overlap for more than 25% of their covered time (`maximumMergedOverlap`) is two subtitles in one file and is rejected. Real subtitles measured at most 1.5%, two stitched episodes 52%. SRT and WebVTT only: ASS/SSA run signs and karaoke beside dialogue.
- A subtitle that fails installation validation logs `subtitle.install_failed` at warn, since it is a recorded rejection, not an operational error. LAPSE still checks the timing of the result.

## Archive and pack selection

### Extraction

- `__MACOSX` folders and AppleDouble `._` entries are ignored before any path or depth check.
- Members may sit up to three folders deep. Only subtitle files count against the 100-file limit; an archive may hold up to 10,000 entries.
- Members whose text, syntax or cue times cannot be read are skipped and counted in `Manifest.SkippedMembers`. Only an archive with no readable member is a `ContentError` (`invalid_subtitle`).
- Extraction fails closed on traversal, links, nested archives and decompression limits.
- Two members with the same name (case-insensitive) are kept once when their normalized text is identical (per-encoding `ANSI/` and `UTF/` copies); different text keeps both (`assignMemberNames`). Each version is prefixed with the fewest trailing folders that make every prefix distinct (`UTF - name.srt`, `Season 1 - Croatian - 01.srt`). A version without a folder, or whose prefixed name is taken or over 255 bytes, keeps the bare name if free, otherwise is numbered (`name (2).srt`). Only a numbered name over 255 bytes rejects the archive.
- Selection logs carry the archive type, subtitle count, selection rule and matching count, never member filenames (`archive.members_selected`, `archive.classified`).

### Episode archives

- `Select` requires a unique match. `SelectAlternatives` allows up to three (`maxEpisodeVersions`) distinct-content versions identified by explicit, consistent target-episode tokens, all by compact numbers, or all by the [release-name number](#release-name-numbers-release_episode_token). Identical checksums collapse first.
- With more matching versions, they are ordered by `match.ReleaseAffinity` (release group 25, source 15, edition 10, streaming service 5, resolution 5 against the media); ties keep name order, and only the first three are timed.
- Versions already rejected for this media (member-scoped) are put after untried ones before the cut (`SelectUntriedAlternatives`). While untried versions remain beyond three, the pack is not recorded as exhausted; a later search downloads it again and times the next ones. The pack cache applies the same lookup (`pack.Eligibility.Rejected`) and records `versions_exhausted` when every selected version is rejected and none is left untried.
- Provider-direct, range and title-only ambiguities are rejected.
- Each version inherits its provider result's score and joins the same prepared tier; shortlist slots stay capped at three. Candidate-wide exhaustion is recorded only after all versions fail deterministically.
- A downloaded archive with no unique member for the episode is recorded as `pack_selection`; the workflow continues through the shortlist without a provider cooldown.

### Movie archives

`SelectMovieAlternatives`:

1. Filter forced subtitles.
2. Drop members whose name words (excluding the movie's own title words) mark extras (commentary, short, trailer, featurette, extras, bonus) when others remain.
3. Prefer members tagged with the requested language, then untagged ones, then other-language tags (two-letter codes count only uppercase or as the last name word).
4. Count identical normalized content as one choice.

One member is `single_movie`; two or three are `movie_versions`, prepared like episode versions (LAPSE-ranked, never score-bypassed, member-scoped rejections). Disc markers (`match.IsMultiDisc`), more than three versions, and several distinct members under an exact-hash result are `pack_selection`.

### Runtime packs

A runtime pack is an episode payload with several normalized members (forced variants included), or one member carrying a valid multi-episode range, without provider-declared `Pack` metadata.

- Extraction records `RuntimePack` in the manifest; provider candidates and identities stay unchanged.
- Every non-exact runtime pack needs LAPSE, cache hits included. Runtime packs with exact-hash metadata never enter the cache, so hash authority cannot transfer between episodes.
- Caching requires a provider series ID or non-empty provider title, and one positive season from provider metadata or consistent strictly parsed member tokens. Unknown, zero, malformed or conflicting seasons disable publication; no requested identity or filename guess fills the gaps.
- A strictly selected cached member supplies the 20-point episode evidence and supersedes only the original episode restriction; all other gates stay.
- Cache publication precedes LAPSE, so one rejected member does not discard its siblings. Writes are best effort.

## Episode tokens

- Tokens accept an optional dot between season and episode (`S01.E02`) and need complete token boundaries. Malformed episode-shaped names cannot use the generic singleton or title-only fallback.
- Ranges are recognized only as complete hyphenated tokens: `S01E01-E03`, same-season `S01E01-S01E03`, same-season `1x01-1x03`, and `SSxEE.EE`. Cross-season, reversed, chained, incomplete and suffix-contaminated forms fail closed. `S01E01.1080p` stays single-episode evidence.
- A bare `E` after a spaced dash and followed by a space starts the episode title (`S01E05 - E and E`). Attached or unspaced forms (`S01E05-E`) and numbered ones (`- E06`) fail closed.

### Compact numbers (`compact_episode`)

A member with no other episode evidence that contains exactly one separated three-digit number `SEE` (seasons 1–9, for example `malcolm.in.the.middle.607.hdtv`) identifies that season and episode.

- 264, 265, 480, 576 and 720, numbers attached to other characters (`x264`, `1080p`, years), and digits in the series title (`9-1-1`) are ignored.
- Numbers are whole digit runs, so `607-608` or `607608` are two numbers and identify no single episode.
- A number equal to the target's absolute number A matches.
- Otherwise the name shape decides first:
  - a leading `[Group]` tag reads as absolute and never matches (a tag naming a site, containing a dot or `www`, is not a group tag);
  - for a series Sonarr types as anime (`domain.Media.SeriesType`), a number after a spaced dash (`One Piece - 607`) reads as absolute and never matches;
  - for other series, and for rows whose series type is not known yet, a spaced dash is ordinary naming: `Malcolm in the Middle - 607.srt` is S06E07;
  - a number between spaced dashes followed by a title starting with a letter (`… - 607 - Hal Coaches`) is TV naming, even for anime, unless a group tag is present.
- For other names, when A is known, a number is accepted only when it is at least 100 past A.
- For a lone generic file, any compact number that does not read as the target is conflicting evidence.

### Numbered members (`numbered_episode`)

Members without episode tokens read as `N-EE …` (the prefix season must equal the target season), or for season 1 `Part N` or a leading `N - Title`. It applies only when at least three members are numbered this way, and the match must be unique.

### Release-name numbers (`release_episode_token`)

The last episode rule, for a show whose Arr numbering differs from how its releases and subtitles are named. Sonarr follows its database, which may number a show as one 16-episode season that releases split into two seasons of 8: the file Sonarr calls S01E10 is released as `Show.S02E02…`, and every subtitle pack names it S02E02.

- The media file's release name (Sonarr's `sceneName`, stored as both `ReleaseName` and `OriginalFilename`) must hold exactly one complete episode token (resolutions such as `1920x1080` do not count) and no range, in a **different season** from Arr's. A release name in Arr's own season is ignored, so it can never pick another episode of the same season.
- The pack must not follow Arr's numbering: if a member names Arr's season with an episode past every episode the pack gives the release's season, the rule does not run. In such a pack the release's number is another episode (Sonarr's own S02E02).
- Members are judged against the release's number instead: a member whose tokens conflict with it, or that carries a range, is skipped; `Select` needs exactly one member naming that episode; `SelectAlternatives` allows up to three versions of it, as for episode tokens.
- It runs only after every rule above found nothing for Arr's number, so a pack that names Arr's number still wins.
- Packs only: `SelectSingleEpisode` does not accept a lone file through it, and the pack cache never serves a member through it.
- A member chosen by it is installed only after LAPSE. When the sync policy could skip LAPSE (`sync.policy: never`, or a single provider-pack member with `sync.lapse_for_packs: false`), the workflow rejects the pack as `pack_selection` instead; the policy is part of the rejection identity, so changing it re-opens the rejection.
- Not covered: a provider pack labelled with the release's season (season 2 for Sonarr's season 1) is rejected by the [identity gates](#identity-gates) before it is downloaded. Packs without a season label, or labelled with Arr's season, reach this rule.

`single_generic` does not apply when extraction skipped a member, since that member may have held the archive's explicit evidence. Forced-only policy applies to every selected member, including a plain one-member movie payload.

## Episode ranges

A file covering several consecutive episodes is a range target (see [Multi-episode files](../reconciliation.md#multi-episode-files)). `match.coversTarget` accepts:

- an exact hash;
- a range pack spanning the target;
- a parsed release whose standard range spans it, or, when the target has absolute numbers, an absolute range such as `66-67` (parsed only from names without a season, span at most 10, never year-like).

Rules:

- Whole-season packs never cover a range: they cannot show a combined member, and downloading them would spend quota on a near-certain `pack_selection` rejection.
- The episode-conflict check accepts any candidate episode inside the range; anything else is rejected at score time.
- `pack.Select` for ranges accepts only a unique filename range covering the target, and any other episode token in the name must fall inside it. A lone non-pack file is accepted when its filename does not contradict the range (`single_range`).
- Before LAPSE and again at installation, a range subtitle must end at or after 75% of the runtime, else `partial_coverage`.
- Providers declaring `SingleEpisodeOnly` (Gestdown) are excluded through `provider.SupportsMedia`. When only such providers could still download, the route gate pauses just the multi-episode files of the route (`RangesPaused`, `RouteKey.RangesOnly`).
- Range fields join the candidate signature only for ranges, so single-episode signatures are unaffected.
