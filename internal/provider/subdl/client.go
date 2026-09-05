package subdl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
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
	seen := map[string]struct{}{}
	for _, parameters := range searches {
		found, err := c.searchOnce(ctx, parameters)
		if err != nil {
			return nil, err
		}
		appendUniqueItems(&items, seen, found)
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
		appendUniqueItems(&items, seen, found)
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
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&decoded); err != nil {
		return nil, &baseprovider.InvalidPayloadError{Message: "SubDL search response is not valid JSON"}
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

func appendUniqueItems(destination *[]searchItem, seen map[string]struct{}, items []searchItem) {
	for _, item := range items {
		key := item.URL
		if key == "" {
			key = item.Name
		}
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		*destination = append(*destination, item)
	}
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
		if title == "" {
			title = query.Media.Title
		}
		if year == 0 {
			year = query.Media.Year
		}
		candidate := domain.Candidate{ProviderID: c.id, ResultID: downloadRef, DownloadRef: downloadRef, Language: language, Kind: query.Media.Ref.Kind, Title: title, Year: year, ExternalIDs: domain.ExternalIDs{IMDb: item.Identity.IMDb, TMDB: item.Identity.TMDB}, Season: item.Season, Episode: item.Episode, ReleaseNames: releases, HearingImpaired: item.Hearing, Rating: min(max(item.Rating, 0), 1), Popularity: baseprovider.NormalizePopularity(item.DownloadCount), DownloadCount: item.DownloadCount}
		if query.Media.Ref.Kind == domain.MediaEpisode {
			from, to := item.EpisodeFrom, item.EpisodeEnd
			if from <= 0 || to <= from {
				from, to = releaseRange(releases)
			}
			isPack := from > 0 && to > from
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
				candidate.HearingImpaired = direct.Hearing
				candidate.ReleaseNames = uniqueStrings(append(candidate.ReleaseNames, direct.ReleaseName))
			} else if isPack {
				candidate.Episode = 0
				candidate.Pack = &domain.PackInfo{Scope: domain.PackRange, Season: item.Season, EpisodeFrom: from, EpisodeTo: to}
			} else if item.FullSeason {
				if item.Season != query.Media.Season {
					continue
				}
				candidate.Episode = 0
				candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: item.Season}
			} else if item.Episode == 0 {
				continue
			} else if item.Season != 0 && item.Season != query.Media.Season || item.Episode != 0 && item.Episode != query.Media.Episode && item.Episode != query.Media.AbsoluteEpisode {
				continue
			}
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func matchingDirect(files []unpackFile, query baseprovider.SearchQuery) (unpackFile, bool) {
	for _, file := range files {
		language, err := fromSubDLLanguage(file.Language)
		if err != nil || !domain.EquivalentLanguage(language, query.Language) {
			continue
		}
		if file.Episode == query.Media.Episode || query.Media.AbsoluteEpisode > 0 && file.Episode == query.Media.AbsoluteEpisode {
			return file, true
		}
	}
	return unpackFile{}, false
}

var episodeRangePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:S\d{1,2})?E(P)?0*(\d{1,4})[-_. ]+E?(?:P)?0*(\d{1,4})`),
	regexp.MustCompile(`(?i)\bEP0*(\d{1,4})[-_. ]+0*(\d{1,4})\b`),
}

func releaseRange(releases []string) (int, int) {
	for _, release := range releases {
		for _, pattern := range episodeRangePatterns {
			match := pattern.FindStringSubmatch(release)
			if len(match) == 4 {
				from, _ := strconv.Atoi(match[2])
				to, _ := strconv.Atoi(match[3])
				return from, to
			}
			if len(match) == 3 {
				from, _ := strconv.Atoi(match[1])
				to, _ := strconv.Atoi(match[2])
				return from, to
			}
		}
	}
	return 0, 0
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
	lower := strings.ToLower(message)
	return strings.Contains(lower, "can't find") || strings.Contains(lower, "not found") || strings.Contains(lower, "no subtitle")
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
	if err != nil || raw == "" || parsed.User != nil || parsed.Host != "" && !parsed.IsAbs() {
		return "", false
	}
	if parsed.IsAbs() && !c.allowedDownloadURL(parsed) {
		return "", false
	}
	if !strings.HasPrefix(parsed.Path, "/") {
		parsed.Path = "/" + parsed.Path
	}
	return parsed.EscapedPath(), true
}

func (c *Client) Download(ctx context.Context, candidate domain.Candidate, writer io.Writer) (baseprovider.DownloadMetadata, error) {
	base, _ := url.Parse(strings.TrimRight(c.config.DownloadBaseURL, "/") + "/")
	reference, err := url.Parse(candidate.DownloadRef)
	if err != nil {
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
