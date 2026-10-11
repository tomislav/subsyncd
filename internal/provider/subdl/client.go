package subdl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/pack"
	baseprovider "subsyncd/internal/provider"
)

type Client struct {
	id        string
	config    Config
	transport baseprovider.Client
	clock     baseprovider.Clock
	// events receives warnings about SubDL answers the client doesn't recognise;
	// nil discards them.
	events *observability.Emitter
}

func New(config Config, transport baseprovider.Client, clock baseprovider.Clock) (*Client, error) {
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = baseprovider.SystemClock{}
	}
	return &Client{id: transport.ProviderID, config: config, transport: transport, clock: clock}, nil
}

func Factory(id string, node yaml.Node, dependencies baseprovider.Dependencies) (baseprovider.Provider, error) {
	payload, err := yaml.Marshal(&node)
	if err != nil {
		return nil, fmt.Errorf("encode SubDL config: %w", err)
	}
	config, err := DecodeConfig(payload)
	if err != nil {
		return nil, err
	}
	clock := dependencies.Clock
	if clock == nil {
		clock = baseprovider.SystemClock{}
	}
	if dependencies.Gate == nil || dependencies.HTTPClient == nil {
		return nil, fmt.Errorf("SubDL HTTP client and provider gate are required")
	}
	dependencies.Gate.Configure(id, config.RequestsPerSecond, config.Burst, config.MaxConcurrent, "subdl")
	transport := baseprovider.Client{HTTP: dependencies.HTTPClient, Gate: dependencies.Gate, Clock: clock, ProviderID: id, ProviderType: "subdl"}
	client, err := New(config, transport, clock)
	if err != nil {
		return nil, err
	}
	client.events = dependencies.Events.For("provider")
	return client, nil
}

func (c *Client) ID() string { return c.id }

func (c *Client) CheckSearchAvailability(ctx context.Context) error {
	return c.transport.CheckSearchAvailability(ctx)
}

func (c *Client) CheckDownloadAvailability(ctx context.Context) error {
	return c.transport.CheckDownloadAvailability(ctx)
}

// RecordDownloadRefusal pauses downloads after one delivered a limit, login
// or client-block message instead of a subtitle, for the cooldown of kind.
func (c *Client) RecordDownloadRefusal(ctx context.Context, kind baseprovider.CooldownKind) error {
	return c.transport.RecordDownloadRefusal(ctx, kind)
}

// v3: searches no longer send hi=1, which made SubDL return hearing-impaired
// subtitles only; cached v2 results are that filtered set.
func (c *Client) SearchCacheVersion() string { return "subdl-no-hi-filter-v3" }

const annotationEvidenceVersion = "subdl-annotations-v1"

func (c *Client) CanReuseCachedCandidate(candidate domain.Candidate) bool {
	return candidate.EvidenceVersion == annotationEvidenceVersion
}

func (c *Client) Capabilities() baseprovider.Capabilities {
	return baseprovider.Capabilities{SeasonPacks: true, DirectPackMember: true}
}

func (c *Client) SupportsLanguage(language domain.Language) bool {
	_, ok := subDLLanguage(language)
	return ok
}

func (c *Client) Search(ctx context.Context, query baseprovider.SearchQuery) ([]domain.Candidate, error) {
	if query.Mode != baseprovider.SearchBroad {
		return nil, fmt.Errorf("SubDL supports broad search only")
	}
	language, ok := subDLLanguage(query.Language)
	if !ok {
		return nil, fmt.Errorf("SubDL does not support language %s", query.Language)
	}
	base := c.searchParameters(query.Media, language)
	var searches []url.Values
	if query.Media.Ref.Kind == domain.MediaEpisode {
		standard := cloneValues(base)
		standard.Set("season_number", strconv.Itoa(query.Media.Season))
		standard.Set("episode_number", strconv.Itoa(query.Media.Episode))
		searches = append(searches, standard)
		if query.Media.AbsoluteEpisode > 0 && query.Media.AbsoluteEpisode != query.Media.Episode {
			absolute := cloneValues(base)
			absolute.Set("episode_number", strconv.Itoa(query.Media.AbsoluteEpisode))
			searches = append(searches, absolute)
		}
		season := cloneValues(base)
		season.Set("season_number", strconv.Itoa(query.Media.Season))
		searches = append(searches, season)
	} else {
		searches = append(searches, base)
	}

	items := make([]searchItem, 0)
	for _, parameters := range searches {
		found, err := c.searchOnce(ctx, parameters)
		if err != nil {
			return nil, err
		}
		items = append(items, found...)
	}
	if len(items) == 0 && query.Media.Ref.Kind == domain.MediaMovie && base.Get("imdb_id") != "" && query.Media.ExternalIDs.TMDB > 0 {
		alternate := cloneValues(base)
		alternate.Del("imdb_id")
		alternate.Set("tmdb_id", strconv.FormatInt(query.Media.ExternalIDs.TMDB, 10))
		found, err := c.searchOnce(ctx, alternate)
		if err != nil {
			return nil, err
		}
		items = append(items, found...)
	}
	if len(items) == 0 && query.Media.Ref.Kind == domain.MediaEpisode && query.Media.Title != "" {
		titleOnly := cloneValues(base)
		titleOnly.Del("imdb_id")
		titleOnly.Del("tmdb_id")
		titleOnly.Del("file_name")
		titleOnly.Set("film_name", query.Media.Title)
		found, err := c.searchOnce(ctx, titleOnly)
		if err != nil {
			return nil, err
		}
		items = append(items, found...)
	}
	return c.normalize(query, items), nil
}

func (c *Client) searchParameters(media domain.Media, language string) url.Values {
	parameters := url.Values{
		"api_key":       {c.config.APIKey},
		"type":          {map[bool]string{true: "tv", false: "movie"}[media.Ref.Kind == domain.MediaEpisode]},
		"languages":     {language},
		"releases":      {"1"},
		"comment":       {"1"},
		"unpack":        {"1"},
		"subs_per_page": {"30"},
		"client":        {"custom_integration"},
	}
	if media.Year > 0 {
		parameters.Set("year", strconv.Itoa(media.Year))
	}
	if media.OriginalFilename != "" {
		parameters.Set("file_name", media.OriginalFilename)
	}
	if media.ExternalIDs.IMDb != "" {
		parameters.Set("imdb_id", media.ExternalIDs.IMDb)
	} else if media.ExternalIDs.TMDB != 0 {
		parameters.Set("tmdb_id", strconv.FormatInt(media.ExternalIDs.TMDB, 10))
	} else if media.OriginalFilename == "" && media.Title != "" {
		parameters.Set("film_name", media.Title)
	}
	if media.Ref.Kind == domain.MediaEpisode {
		parameters.Set("full_season", "1")
	}
	return parameters
}

type searchResponse struct {
	Status    *bool         `json:"status"`
	Success   *bool         `json:"success"`
	Error     string        `json:"error"`
	Results   []mediaResult `json:"results"`
	Subtitles []searchItem  `json:"subtitles"`
}

type mediaResult struct {
	SDID int64  `json:"sd_id"`
	IMDb string `json:"imdb_id"`
	TMDB int64  `json:"tmdb_id"`
	Name string `json:"name"`
	Year int    `json:"year"`
}

type searchItem struct {
	Comment       string       `json:"comment"`
	Name          string       `json:"name"`
	URL           string       `json:"url"`
	Language      string       `json:"language"`
	Season        int          `json:"season"`
	Episode       int          `json:"episode"`
	EpisodeFrom   int          `json:"episode_from"`
	EpisodeEnd    int          `json:"episode_end"`
	FullSeason    bool         `json:"full_season"`
	ReleaseName   string       `json:"release_name"`
	Releases      []string     `json:"releases"`
	Hearing       bool         `json:"hi"`
	Rating        float64      `json:"rating"`
	DownloadCount int64        `json:"download_count"`
	UnpackFiles   []unpackFile `json:"unpack_files"`
	Identity      mediaResult  `json:"-"`
}

type unpackFile struct {
	Comment     string `json:"comment"`
	FileID      string `json:"file_n_id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	ReleaseName string `json:"release_name"`
	Language    string `json:"language"`
	Season      int    `json:"season"`
	Episode     int    `json:"episode"`
	Hearing     bool   `json:"hi"`
}

func (c *Client) searchOnce(ctx context.Context, parameters url.Values) ([]searchItem, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.BaseURL+"?"+parameters.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("create SubDL search request")
	}
	response, err := c.transport.Do(ctx, baseprovider.OperationSearch, request)
	if err != nil {
		if response != nil {
			defer response.Body.Close()
			if response.StatusCode == http.StatusTooManyRequests {
				return nil, c.decodeLimit(ctx, baseprovider.OperationSearch, response.Body, response.Header, err)
			}
		}
		return nil, c.rejectedCredentials(ctx, baseprovider.OperationSearch, response, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if err := c.refusal(ctx, baseprovider.OperationSearch, response); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("SubDL search returned HTTP %d", response.StatusCode)
	}
	var decoded searchResponse
	if err := baseprovider.DecodeJSON(response.Body, &decoded, 4<<20, "SubDL search response is not valid JSON"); err != nil {
		return nil, err
	}
	if (decoded.Status != nil && !*decoded.Status) || (decoded.Success != nil && !*decoded.Success) {
		if isNoResult(decoded.Error) {
			return nil, nil
		}
		if limitErr := c.payloadLimit(ctx, baseprovider.OperationSearch, decoded.Error, time.Time{}); limitErr != nil {
			return nil, limitErr
		}
		// Any other status-false answer is treated as an empty result rather than a
		// failed search: SubDL rewords its not-found message from time to time, and
		// a failure would put no-result titles on the technical retry schedule. The
		// warning carries a short, sanitized excerpt so a new wording is visible;
		// SubDL's text is kept out of the error, whose wording classifies failures.
		attrs := []slog.Attr{slog.String("provider", c.id), slog.String("operation", string(baseprovider.OperationSearch))}
		if excerpt := rejectionExcerpt(decoded.Error, c.config.APIKey); excerpt != "" {
			attrs = append(attrs, slog.String("excerpt", excerpt))
		}
		if isCredentialProblem(decoded.Error) {
			// A bad or missing key must stay a visible failure, not an empty library.
			c.events.Log(ctx, slog.LevelWarn, "provider.response_unrecognized", "SubDL rejected the search with a credential message", attrs...)
			return nil, fmt.Errorf("SubDL search was rejected")
		}
		c.events.Log(ctx, slog.LevelWarn, "provider.response_unrecognized", "SubDL answered with an unrecognised message; treating it as no result", attrs...)
		return nil, nil
	}
	// A search that matched several titles lists subtitles of all of them, so
	// no one title is the subtitles' identity.
	if identity, ok := singleTitle(decoded.Results); ok {
		for index := range decoded.Subtitles {
			decoded.Subtitles[index].Identity = identity
		}
	}
	return decoded.Subtitles, nil
}

// singleTitle returns the title a search matched when it matched exactly one
// distinct title (by SubDL ID, then IMDb, TMDB, or name and year).
func singleTitle(results []mediaResult) (mediaResult, bool) {
	distinct := map[string]bool{}
	for _, result := range results {
		key := ""
		switch {
		case result.SDID != 0:
			key = "sd:" + strconv.FormatInt(result.SDID, 10)
		case result.IMDb != "":
			key = "imdb:" + strings.ToLower(result.IMDb)
		case result.TMDB != 0:
			key = "tmdb:" + strconv.FormatInt(result.TMDB, 10)
		default:
			key = "name:" + strings.ToLower(strings.TrimSpace(result.Name)) + "|" + strconv.Itoa(result.Year)
		}
		distinct[key] = true
	}
	if len(distinct) != 1 {
		return mediaResult{}, false
	}
	return results[0], true
}

func (c *Client) normalize(query baseprovider.SearchQuery, items []searchItem) []domain.Candidate {
	var candidates []domain.Candidate
	for _, item := range items {
		language, err := fromSubDLLanguage(item.Language)
		if err != nil || !domain.EquivalentLanguage(language, query.Language) {
			continue
		}
		downloadRef, ok := c.downloadReference(item.URL)
		if !ok {
			continue
		}
		releases := uniqueStrings(append(append([]string{}, item.Releases...), item.ReleaseName))
		title, year := item.Identity.Name, item.Identity.Year
		candidate := domain.Candidate{EvidenceVersion: annotationEvidenceVersion, ProviderID: c.id, ResultID: downloadRef, DownloadRef: downloadRef, Language: language, Kind: query.Media.Ref.Kind, Title: title, Year: year, ExternalIDs: domain.ExternalIDs{IMDb: item.Identity.IMDb, TMDB: item.Identity.TMDB}, Season: item.Season, Episode: item.Episode, ReleaseNames: releases, HearingImpaired: item.Hearing, Rating: min(max(item.Rating, 0), 1), Popularity: baseprovider.NormalizePopularity(item.DownloadCount), DownloadCount: item.DownloadCount}
		candidate.Forced, candidate.HearingImpaired = baseprovider.Annotations(item.Name, item.Comment, item.Hearing)
		if query.Media.Ref.Kind == domain.MediaEpisode {
			from, to := item.EpisodeFrom, item.EpisodeEnd
			rangeSeason, releaseFrom, releaseTo, invalid := releaseRange(releases)
			if invalid || rangeSeason != 0 && rangeSeason != query.Media.Season {
				continue
			}
			if from <= 0 || to <= from {
				from, to = releaseFrom, releaseTo
			}
			isPack := from > 0 && to > from
			if item.Season != 0 && item.Season != query.Media.Season {
				continue
			}
			if isPack && !containsEpisode(from, to, query.Media.Episode, query.Media.AbsoluteEpisode) {
				continue
			}
			if direct, found := matchingDirect(item.UnpackFiles, query); found {
				ref, safe := c.downloadReference(direct.URL)
				if !safe {
					continue
				}
				candidate.ResultID = downloadRef + ":" + direct.FileID
				candidate.DownloadRef = ref
				candidate.Season = direct.Season
				candidate.Episode = direct.Episode
				forced, hearing := baseprovider.Annotations(direct.Name, direct.Comment, direct.Hearing)
				candidate.Forced = candidate.Forced || forced
				candidate.HearingImpaired = candidate.HearingImpaired || hearing
				candidate.ReleaseNames = uniqueStrings(append(candidate.ReleaseNames, direct.ReleaseName))
			} else if isPack {
				candidate.Episode = 0
				candidate.Pack = &domain.PackInfo{Scope: domain.PackRange, Season: item.Season, EpisodeFrom: from, EpisodeTo: to}
				if !(from <= query.Media.Episode && query.Media.Episode <= to) && query.Media.AbsoluteEpisode > 0 {
					candidate.Pack.EpisodeFrom, candidate.Pack.EpisodeTo = 0, 0
					candidate.Pack.AbsoluteEpisodeFrom, candidate.Pack.AbsoluteEpisodeTo = from, to
				}
			} else if item.FullSeason {
				if item.Season != query.Media.Season {
					continue
				}
				candidate.Episode = 0
				candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: item.Season}
			} else if item.Season != 0 && item.Season != query.Media.Season || item.Episode != 0 && item.Episode != query.Media.Episode && item.Episode != query.Media.AbsoluteEpisode {
				continue
			}
		}
		if query.Media.Ref.Kind == domain.MediaEpisode && candidate.Episode != query.Media.Episode && candidate.Episode == query.Media.AbsoluteEpisode && candidate.Episode > 0 {
			candidate.AbsoluteEpisode = candidate.Episode
			candidate.Episode = 0
		}
		candidates = append(candidates, candidate)
	}
	candidates = baseprovider.DeduplicateCandidates(candidates)
	eligible := candidates[:0]
	for _, candidate := range candidates {
		// A later duplicate may supply absent episode coordinates. Apply the
		// unknown-episode gate only after its evidence has been merged.
		if query.Media.Ref.Kind == domain.MediaEpisode && candidate.Episode == 0 && candidate.AbsoluteEpisode == 0 && candidate.Pack == nil {
			continue
		}
		eligible = append(eligible, candidate)
	}
	return eligible
}

func matchingDirect(files []unpackFile, query baseprovider.SearchQuery) (unpackFile, bool) {
	var selected unpackFile
	count := 0
	for _, file := range files {
		language, err := fromSubDLLanguage(file.Language)
		if err != nil || !domain.EquivalentLanguage(language, query.Language) || file.Season != 0 && file.Season != query.Media.Season {
			continue
		}
		if file.Episode == query.Media.Episode || query.Media.AbsoluteEpisode > 0 && file.Episode == query.Media.AbsoluteEpisode {
			selected = file
			count++
		}
	}
	return selected, count == 1
}

func releaseRange(releases []string) (season, from, to int, invalid bool) {
	for _, release := range releases {
		s, f, t, found, bad := pack.ReleaseEpisodeRange(release)
		if bad {
			return 0, 0, 0, true
		}
		if found {
			if from != 0 && (season != s || from != f || to != t) {
				return 0, 0, 0, true
			}
			season, from, to = s, f, t
		}
	}
	return
}

func containsEpisode(from, to, standard, absolute int) bool {
	return from <= standard && standard <= to || absolute > 0 && from <= absolute && absolute <= to
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func cloneValues(source url.Values) url.Values {
	copy := make(url.Values, len(source))
	for key, values := range source {
		copy[key] = append([]string(nil), values...)
	}
	return copy
}

// maxRejectionExcerpt bounds how much of an unrecognised SubDL message a warning
// may carry, not counting the truncation marker.
const maxRejectionExcerpt = 80

// keySentinel stands in for the configured API key while the message is
// filtered; it is a private-use rune, so SubDL text cannot produce it.
const keySentinel = '\uE000'

// rejectionExcerpt reduces an unrecognised SubDL message to a short, single-line,
// plain-text excerpt for a log field. Event fields bypass the error redaction, so
// it redacts on its own: the configured API key, and any token that looks like a
// credential (mixed letters and digits of 16+, or a 24+ alphanumeric run). It
// keeps letters, digits and basic punctuation, collapses whitespace, never cuts
// a "[redacted]" marker, and ends a shortened excerpt with "…". It returns ""
// when nothing readable remains.
func rejectionExcerpt(message, apiKey string) string {
	if apiKey != "" {
		message = strings.ReplaceAll(message, apiKey, " "+string(keySentinel)+" ")
	}
	message = strings.Map(func(character rune) rune {
		switch {
		case character == keySentinel, unicode.IsLetter(character), unicode.IsDigit(character):
			return character
		case strings.ContainsRune(".,:;'!?()-_%", character):
			return character
		default:
			return ' '
		}
	}, observability.SafeText(message))
	var tokens []string
	readable := false
	for _, token := range strings.Fields(message) {
		if strings.ContainsRune(token, keySentinel) || looksLikeCredential(token) {
			token = "[redacted]"
		} else if strings.IndexFunc(token, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
			readable = true
		}
		tokens = append(tokens, token)
	}
	if !readable && !containsToken(tokens, "[redacted]") {
		return ""
	}
	var excerpt strings.Builder
	length := 0
	for i, token := range tokens {
		width := utf8.RuneCountInString(token)
		if i > 0 {
			width++
		}
		if length+width > maxRejectionExcerpt {
			if length == 0 && token != "[redacted]" {
				excerpt.WriteString(string([]rune(token)[:maxRejectionExcerpt]))
			}
			excerpt.WriteString("\u2026")
			return excerpt.String()
		}
		if i > 0 {
			excerpt.WriteByte(' ')
		}
		excerpt.WriteString(token)
		length += width
	}
	return excerpt.String()
}

// looksLikeCredential reports a token that could be a key or token echoed back
// in another form: 16+ letters and digits mixing both (ignoring separators), or
// an unbroken alphanumeric run of 24+.
func looksLikeCredential(token string) bool {
	letters, digits, run, longest := 0, 0, 0, 0
	for _, character := range token {
		switch {
		case unicode.IsLetter(character):
			letters++
			run++
		case unicode.IsDigit(character):
			digits++
			run++
		default:
			run = 0
		}
		if run > longest {
			longest = run
		}
	}
	return (letters > 0 && digits > 0 && letters+digits >= 16) || longest >= 24
}

func containsToken(tokens []string, want string) bool {
	for _, token := range tokens {
		if token == want {
			return true
		}
	}
	return false
}

// isCredentialProblem reports a status-false message about the API key or
// authorisation, which must fail the search instead of reading as no result.
func isCredentialProblem(message string) bool {
	message = strings.ToLower(message)
	for _, marker := range []string{"api key", "api_key", "apikey", "unauthor", "forbidden", "token"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// isNoResult reports SubDL's not-found messages, ignoring case, surrounding and
// repeated whitespace, curly apostrophes and trailing punctuation.
func isNoResult(message string) bool {
	normalized := strings.NewReplacer("\u2019", "'", "\u2018", "'").Replace(strings.ToLower(message))
	normalized = strings.TrimRight(strings.Join(strings.Fields(normalized), " "), ".!")
	switch strings.TrimSpace(normalized) {
	case "can't find film", "can't find movie or tv", "film not found", "no subtitles", "no subtitles found", "no subtitle found":
		return true
	default:
		return false
	}
}

func (c *Client) decodeLimit(ctx context.Context, operation baseprovider.Operation, body io.Reader, headers http.Header, fallback error) error {
	var cooldown *baseprovider.CooldownError
	if !errors.As(fallback, &cooldown) {
		return fallback
	}
	var payload struct {
		Error string `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&payload) == nil {
		var reset time.Time
		if window, found := baseprovider.ParseRateLimit(c.clock.Now(), headers); found && window.ResetAt.After(c.clock.Now()) {
			reset = window.ResetAt
		}
		if err := c.payloadLimit(ctx, operation, payload.Error, reset); err != nil {
			return err
		}
	}
	return fallback
}

// limitCode classifies a SubDL error code or message as a limit: its own codes,
// wording naming a limit or quota that was reached, exceeded or exhausted, or
// too many requests. A reached limit that names the day, downloads or a quota
// ("You have reached your daily download limit", "API quota exceeded",
// "Quota exhausted") is the daily limit; any other ("Request limit reached",
// "You have reached your limit") and too many requests are a rate limit, a
// short cooldown rather than a pause until the daily reset. Other mentions
// of a limit ("must not exceed the limit of 30", "unlimited downloads") and
// other things exceeded ("Max file size exceeded for download") are not
// limits. It returns "" for anything else.
func limitCode(message string) string {
	normalized := strings.ToLower(strings.TrimSpace(message))
	switch normalized {
	case "daily_limit", "api_download_limit_exceeded", "service_busy", "rate_limit":
		return normalized
	}
	limited := strings.ReplaceAll(normalized, "unlimited", "")
	switch {
	case strings.Contains(normalized, "too many requests") || strings.Contains(normalized, "rate limit"):
		return "rate_limit"
	case !containsAny(limited, "limit", "quota") || !containsAny(normalized, "reached", "exceeded", "exhausted"):
		return ""
	case containsAny(limited, "daily", "download", "quota"):
		return "daily_limit"
	}
	return "rate_limit"
}

func containsAny(message string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// refusal classifies a non-success SubDL answer by its JSON message: a limit
// becomes the usual quota or cooldown, and a 403 is a rejected API key, which
// disables the provider, only when the message says the key or token is
// invalid, banned, revoked, expired or unauthorized. Any
// other 403 is treated as a temporary block and opens the provider's
// transient-failure circuit. It returns nil for other answers.
func (c *Client) refusal(ctx context.Context, operation baseprovider.Operation, response *http.Response) error {
	message := refusalMessage(response.Body)
	if err := c.payloadLimit(ctx, operation, message, time.Time{}); err != nil {
		return err
	}
	if response.StatusCode != http.StatusForbidden {
		return nil
	}
	if invalidAPIKey(message) {
		if err := c.transport.DisableAuthentication(ctx, "SubDL API key rejected (HTTP 403)"); err != nil {
			return fmt.Errorf("disable SubDL provider: %w", err)
		}
		return &baseprovider.AuthenticationError{Message: "API key rejected"}
	}
	return c.temporaryBlock(ctx, operation, "http_403")
}

// refusalMessage reads a SubDL error body's JSON "error" and "message", or ""
// when the body is empty or not such JSON.
func refusalMessage(body io.Reader) string {
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if body == nil || json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.Error + " " + payload.Message)
}

// temporaryBlock opens the transient-failure circuit for a refusal that is
// neither a limit nor a rejected key, so it backs off 1, 5, 15 then 60
// minutes while it lasts.
func (c *Client) temporaryBlock(ctx context.Context, operation baseprovider.Operation, reason string) error {
	state, err := c.transport.Gate.RecordTransientFailure(ctx, c.id, operation, reason, time.Time{})
	if err != nil {
		return err
	}
	return &baseprovider.CooldownError{ProviderID: c.id, Scope: operation, Reason: state.Reason, ResetAt: state.ResetAt}
}

// invalidAPIKey reports a message saying the API key or token is invalid,
// wrong, missing, banned, revoked, expired or unauthorized. It must name the
// API key, or a token other than a download token ("Download token expired"
// is about one link, not the account).
func invalidAPIKey(message string) bool {
	message = strings.ToLower(message)
	apiKey := containsAny(message, "api key", "api_key", "apikey", "api-key")
	if !apiKey && !(strings.Contains(message, "token") && !strings.Contains(message, "download")) {
		return false
	}
	return containsAny(message, "invalid", "not valid", "wrong", "incorrect", "missing", "required", "expired", "revoked", "disabled", "banned", "blocked", "suspended", "unauthorized", "unauthorised", "not authorized", "unknown")
}

// rejectedCredentials handles an HTTP 401 from SubDL's own API or download
// host by its message, like a 403: a limit message is the usual quota or
// cooldown; a message naming the key as rejected, a bare "Unauthorized", or an
// empty or non-JSON body is a key SubDL does not accept and disables the
// provider; any other message opens the transient-failure circuit. A 401
// from another host a download was redirected to (a CDN) says nothing about
// the key and fails only that download.
func (c *Client) rejectedCredentials(ctx context.Context, operation baseprovider.Operation, response *http.Response, err error) error {
	var authentication *baseprovider.AuthenticationError
	if !errors.As(err, &authentication) {
		return err
	}
	if !c.ownHost(response) {
		return fmt.Errorf("SubDL download returned HTTP %d from a redirect target", http.StatusUnauthorized)
	}
	var message string
	if response != nil {
		message = refusalMessage(response.Body)
	}
	if limitErr := c.payloadLimit(ctx, operation, message, time.Time{}); limitErr != nil {
		return limitErr
	}
	if message != "" && !invalidAPIKey(message) && !bareUnauthorized(message) {
		return c.temporaryBlock(ctx, operation, "http_401")
	}
	if disableErr := c.transport.DisableAuthentication(ctx, "SubDL API key rejected (HTTP 401)"); disableErr != nil {
		return fmt.Errorf("disable SubDL provider: %w", disableErr)
	}
	return &baseprovider.AuthenticationError{Message: "API key rejected"}
}

// bareUnauthorized reports a message that only restates the 401 status
// ("Unauthorized", "Not authorized."), which SubDL answers for a key it does
// not accept.
func bareUnauthorized(message string) bool {
	switch strings.TrimRight(strings.Join(strings.Fields(strings.ToLower(message)), " "), ".!") {
	case "unauthorized", "unauthorised", "not authorized", "not authorised", "401 unauthorized":
		return true
	}
	return false
}

// ownHost reports whether a response came from the host of SubDL's
// configured API or download base URL rather than from a redirect target.
func (c *Client) ownHost(response *http.Response) bool {
	if response == nil || response.Request == nil || response.Request.URL == nil {
		return true
	}
	for _, raw := range []string{c.config.BaseURL, c.config.DownloadBaseURL} {
		if base, err := url.Parse(raw); err == nil && strings.EqualFold(base.Host, response.Request.URL.Host) {
			return true
		}
	}
	return false
}

func (c *Client) payloadLimit(ctx context.Context, operation baseprovider.Operation, code string, reset time.Time) error {
	switch limitCode(code) {
	case "daily_limit", "api_download_limit_exceeded":
		if reset.IsZero() {
			reset = baseprovider.FallbackReset(c.clock.Now(), "subdl", baseprovider.CooldownDownloadQuota)
		}
		reset, err := c.transport.PersistCooldown(ctx, operation, baseprovider.CooldownDownloadQuota, reset)
		if err != nil {
			return err
		}
		return &baseprovider.QuotaError{Scope: operation, ResetAt: reset, Message: "SubDL daily limit"}
	case "service_busy":
		if reset.IsZero() {
			reset = baseprovider.FallbackReset(c.clock.Now(), "subdl", baseprovider.CooldownServiceBusy)
		}
		reset, err := c.transport.PersistCooldown(ctx, operation, baseprovider.CooldownServiceBusy, reset)
		if err != nil {
			return err
		}
		return &baseprovider.CooldownError{ProviderID: c.id, Scope: operation, Reason: string(baseprovider.CooldownServiceBusy), ResetAt: reset}
	case "rate_limit":
		if reset.IsZero() {
			reset = baseprovider.FallbackReset(c.clock.Now(), "subdl", baseprovider.CooldownRateLimit)
		}
		reset, err := c.transport.PersistCooldown(ctx, operation, baseprovider.CooldownRateLimit, reset)
		if err != nil {
			return err
		}
		return &baseprovider.CooldownError{ProviderID: c.id, Scope: operation, Reason: string(baseprovider.CooldownRateLimit), ResetAt: reset}
	}
	return nil
}

func (c *Client) downloadReference(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || !c.validDownloadReference(parsed) {
		return "", false
	}
	if !strings.HasPrefix(parsed.Path, "/") {
		parsed.Path = "/" + parsed.Path
	}
	return parsed.EscapedPath(), true
}

// A reference must identify a resource, never just the provider's root page.
func (c *Client) validDownloadReference(reference *url.URL) bool {
	if reference == nil || strings.TrimSpace(reference.Path) == "" || path.Clean("/"+reference.Path) == "/" || reference.User != nil || reference.Host != "" && !reference.IsAbs() {
		return false
	}
	return !reference.IsAbs() || c.allowedDownloadURL(reference)
}

func (c *Client) Download(ctx context.Context, candidate domain.Candidate, writer io.Writer) (baseprovider.DownloadMetadata, error) {
	base, _ := url.Parse(strings.TrimRight(c.config.DownloadBaseURL, "/") + "/")
	reference, err := url.Parse(candidate.DownloadRef)
	if err != nil || !c.validDownloadReference(reference) {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("SubDL candidate has invalid download reference")
	}
	endpoint := base.ResolveReference(reference)
	if !c.allowedDownloadURL(endpoint) {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("SubDL download URL is not allowed")
	}
	query := endpoint.Query()
	query.Set("api_key", c.config.APIKey)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("create SubDL download request")
	}
	request.Header.Set("x-api-key", c.config.APIKey)
	safeHTTP := *c.transport.HTTP
	safeHTTP.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= baseprovider.MaxDownloadRedirects {
			return &baseprovider.RedirectError{Message: "SubDL download redirected too many times"}
		}
		if !c.allowedDownloadURL(request.URL) {
			return &baseprovider.RedirectError{Message: "SubDL redirect target is not allowed"}
		}
		return nil
	}
	transport := c.transport
	transport.HTTP = &safeHTTP
	response, err := transport.Do(ctx, baseprovider.OperationDownload, request)
	if err != nil {
		if response != nil {
			defer response.Body.Close()
			if response.StatusCode == http.StatusTooManyRequests {
				return baseprovider.DownloadMetadata{}, c.decodeLimit(ctx, baseprovider.OperationDownload, response.Body, response.Header, err)
			}
		}
		return baseprovider.DownloadMetadata{}, c.rejectedCredentials(ctx, baseprovider.OperationDownload, response, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if err := c.refusal(ctx, baseprovider.OperationDownload, response); err != nil {
			return baseprovider.DownloadMetadata{}, err
		}
		return baseprovider.DownloadMetadata{}, fmt.Errorf("SubDL download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > c.config.MaxDownloadBytes {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("SubDL download exceeds %d bytes", c.config.MaxDownloadBytes)
	}
	written, err := io.Copy(writer, io.LimitReader(response.Body, c.config.MaxDownloadBytes+1))
	if err != nil {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("stream SubDL download: %w", err)
	}
	if written > c.config.MaxDownloadBytes {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("SubDL download exceeds %d bytes", c.config.MaxDownloadBytes)
	}
	return baseprovider.DownloadMetadata{Filename: path.Base(endpoint.Path), ContentType: response.Header.Get("Content-Type")}, nil
}

func (c *Client) allowedDownloadURL(endpoint *url.URL) bool {
	if endpoint == nil || endpoint.User != nil || (endpoint.Scheme != "https" && !(c.config.AllowInsecureForTests && endpoint.Scheme == "http")) {
		return false
	}
	for _, host := range c.config.DownloadHosts {
		if strings.EqualFold(endpoint.Host, host) || strings.EqualFold(endpoint.Hostname(), host) {
			return true
		}
	}
	return false
}
