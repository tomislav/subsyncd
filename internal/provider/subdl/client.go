package subdl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
	"subsyncd/internal/pack"
	baseprovider "subsyncd/internal/provider"
)

type Client struct {
	id        string
	config    Config
	transport baseprovider.Client
	clock     baseprovider.Clock
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
	return New(config, transport, clock)
}

func (c *Client) ID() string { return c.id }

func (c *Client) SearchCacheVersion() string { return "subdl-annotations-alternate-id-v2" }

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
		"hi":            {"1"},
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
				return nil, c.decodeLimit(ctx, baseprovider.OperationSearch, response.Body, err)
			}
		}
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden {
		if err := c.transport.DisableAuthentication(ctx, "SubDL API key rejected (HTTP 403)"); err != nil {
			return nil, fmt.Errorf("disable SubDL provider: %w", err)
		}
		return nil, &baseprovider.AuthenticationError{Message: "API key rejected"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
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
		if limitErr := c.payloadLimit(ctx, baseprovider.OperationSearch, decoded.Error); limitErr != nil {
			return nil, limitErr
		}
		return nil, fmt.Errorf("SubDL search was rejected")
	}
	if len(decoded.Results) != 0 {
		for index := range decoded.Subtitles {
			decoded.Subtitles[index].Identity = decoded.Results[0]
		}
	}
	return decoded.Subtitles, nil
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
		candidate.Forced, candidate.HearingImpaired = annotations(item.Name, item.Comment, item.Hearing)
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
				forced, hearing := annotations(direct.Name, direct.Comment, direct.Hearing)
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

func isNoResult(message string) bool {
	switch strings.ToLower(strings.TrimSpace(message)) {
	case "can't find film", "film not found", "no subtitles", "no subtitles found", "no subtitle found":
		return true
	default:
		return false
	}
}

func (c *Client) decodeLimit(ctx context.Context, operation baseprovider.Operation, body io.Reader, fallback error) error {
	var payload struct {
		Error string `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&payload) == nil {
		if err := c.payloadLimit(ctx, operation, payload.Error); err != nil {
			return err
		}
	}
	return fallback
}

func (c *Client) payloadLimit(ctx context.Context, operation baseprovider.Operation, code string) error {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "daily_limit", "api_download_limit_exceeded":
		reset := baseprovider.FallbackReset(c.clock.Now(), "subdl", baseprovider.CooldownDownloadQuota)
		if err := c.transport.PersistCooldown(ctx, operation, baseprovider.CooldownDownloadQuota, reset); err != nil {
			return err
		}
		return &baseprovider.QuotaError{Scope: operation, ResetAt: reset, Message: "SubDL daily limit"}
	case "service_busy":
		reset := baseprovider.FallbackReset(c.clock.Now(), "subdl", baseprovider.CooldownServiceBusy)
		if err := c.transport.PersistCooldown(ctx, operation, baseprovider.CooldownServiceBusy, reset); err != nil {
			return err
		}
		return &baseprovider.CooldownError{ProviderID: c.id, Scope: operation, Reason: string(baseprovider.CooldownServiceBusy), ResetAt: reset}
	case "rate_limit":
		reset := baseprovider.FallbackReset(c.clock.Now(), "subdl", baseprovider.CooldownRateLimit)
		if err := c.transport.PersistCooldown(ctx, operation, baseprovider.CooldownRateLimit, reset); err != nil {
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
	safeHTTP.CheckRedirect = func(request *http.Request, _ []*http.Request) error {
		if !c.allowedDownloadURL(request.URL) {
			return fmt.Errorf("SubDL redirect target is not allowed")
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
				return baseprovider.DownloadMetadata{}, c.decodeLimit(ctx, baseprovider.OperationDownload, response.Body, err)
			}
		}
		return baseprovider.DownloadMetadata{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden {
		if err := c.transport.DisableAuthentication(ctx, "SubDL API key rejected (HTTP 403)"); err != nil {
			return baseprovider.DownloadMetadata{}, fmt.Errorf("disable SubDL provider: %w", err)
		}
		return baseprovider.DownloadMetadata{}, &baseprovider.AuthenticationError{Message: "API key rejected"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
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

// Interpret explicit annotation tokens only. Scope negation to each marker;
// filename punctuation separates words, while prose punctuation separates clauses.
// Neither negative prose nor absent member flags erase structured HI evidence.
func annotations(name, comment string, structuredHI bool) (forced, hearing bool) {
	hearing = structuredHI
	for sourceIndex, source := range []string{name, comment} {
		clauses := []string{strings.ToLower(source)}
		if sourceIndex == 1 {
			clauses = strings.FieldsFunc(clauses[0], func(r rune) bool { return r == ';' || r == '\n' || r == '.' || r == ',' })
		}
		for _, clause := range clauses {
			words := strings.FieldsFunc(clause, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
			if strings.Contains(strings.Join(words, " "), "not sure") {
				continue
			}
			type marker struct {
				start, end int
				forced     bool
			}
			var markers []marker
			for i, word := range words {
				switch {
				case word == "forced":
					markers = append(markers, marker{i, i + 1, true})
				case word == "sdh" || sourceIndex == 0 && word == "hi":
					markers = append(markers, marker{i, i + 1, false})
				case word == "hearing" && i+1 < len(words) && words[i+1] == "impaired":
					markers = append(markers, marker{i, i + 2, false})
				}
			}
			previousEnd := 0
			previousNegative := false
			for index, m := range markers {
				negative := false
				inheritedNegative := previousNegative
				for _, word := range words[previousEnd:m.start] {
					switch word {
					case "with", "but":
						negative, inheritedNegative = false, false
					case "and", "or":
						negative = negative || inheritedNegative
					case "no", "not", "non", "without", "remove", "exclude", "strip", "removed", "excluded", "stripped":
						negative = true
					}
				}
				nextStart := len(words)
				if index+1 < len(markers) {
					nextStart = markers[index+1].start
				}
			suffix:
				for _, word := range words[m.end:nextStart] {
					switch word {
					case "with", "without", "but", "and", "or", "no", "not", "non":
						break suffix
					case "removed", "stripped", "excluded", "free":
						negative = true
					}
				}
				if !negative {
					if m.forced {
						forced = true
					} else {
						hearing = true
					}
				}
				previousEnd = m.end
				previousNegative = negative
			}
		}
	}
	return
}
