package opensubtitles

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
	baseprovider "subsyncd/internal/provider"
)

type Client struct {
	id         string
	config     Config
	transport  baseprovider.Client
	hasher     Hasher
	hashCache  HashCache
	clock      baseprovider.Clock
	mu         sync.Mutex
	apiBaseURL string
	token      string
	expiresAt  time.Time
}

type HashCache interface {
	GetMediaHash(context.Context, domain.Media, string) (string, int64, bool, error)
	PutMediaHash(context.Context, domain.Media, string, string, int64) error
}

func New(config Config, transport baseprovider.Client, hasher Hasher, hashCache HashCache, clock baseprovider.Clock) (*Client, error) {
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if hasher == nil {
		hasher = FileHasher{}
	}
	if clock == nil {
		clock = baseprovider.SystemClock{}
	}
	return &Client{id: transport.ProviderID, config: config, transport: transport, hasher: hasher, hashCache: hashCache, clock: clock}, nil
}

func Factory(id string, node yaml.Node, dependencies baseprovider.Dependencies) (baseprovider.Provider, error) {
	payload, err := yaml.Marshal(&node)
	if err != nil {
		return nil, fmt.Errorf("encode OpenSubtitles config: %w", err)
	}
	config, err := DecodeConfig(payload)
	if err != nil {
		return nil, err
	}
	clock := dependencies.Clock
	if clock == nil {
		clock = baseprovider.SystemClock{}
	}
	if dependencies.Gate == nil || dependencies.HTTPClient == nil || dependencies.Store == nil {
		return nil, fmt.Errorf("OpenSubtitles HTTP client, provider gate, and store are required")
	}
	dependencies.Gate.Configure(id, config.RequestsPerSecond, config.Burst, config.MaxConcurrent, "opensubtitles")
	transport := baseprovider.Client{HTTP: dependencies.HTTPClient, Gate: dependencies.Gate, Clock: clock, ProviderID: id, ProviderType: "opensubtitles"}
	return New(config, transport, FileHasher{}, dependencies.Store.Repository(), clock)
}

func (c *Client) ID() string { return c.id }

func (c *Client) CheckSearchAvailability(ctx context.Context) error {
	return c.transport.CheckSearchAvailability(ctx)
}

func (c *Client) CheckDownloadAvailability(ctx context.Context) error {
	return c.transport.CheckDownloadAvailability(ctx)
}

// SearchCacheVersion refreshes legacy filters, language aliases and download evidence.
func (c *Client) SearchCacheVersion() string { return "opensubtitles-evidence-v2" }

func (c *Client) Capabilities() baseprovider.Capabilities {
	return baseprovider.Capabilities{ExactFileHash: true}
}

func (c *Client) SupportsLanguage(language domain.Language) bool {
	_, err := domain.ParseLanguage(language.String())
	return err == nil
}

func (c *Client) Search(ctx context.Context, query baseprovider.SearchQuery) ([]domain.Candidate, error) {
	parameters := url.Values{}
	parameters.Set("languages", openSubtitlesLanguage(query.Language))
	parameters.Set("ai_translated", "exclude")
	parameters.Set("machine_translated", "exclude")
	if query.Mode == baseprovider.SearchExactHash {
		hash, err := c.fileHash(ctx, query.Media)
		if err != nil {
			return nil, err
		}
		parameters.Set("moviehash", hash.MovieHash)
		parameters.Set("moviebytesize", strconv.FormatInt(hash.ByteSize, 10))
	} else {
		setBroadParameters(parameters, query.Media)
	}

	var candidates []domain.Candidate
	for page := 1; ; page++ {
		parameters.Set("page", strconv.Itoa(page))
		var response searchResponse
		if err := c.getJSONWithRefresh(ctx, "/subtitles?"+parameters.Encode(), &response); err != nil {
			return nil, err
		}
		candidates = append(candidates, normalizeCandidates(c.id, query, response.Data)...)
		if response.TotalPages <= page || response.TotalPages == 0 {
			break
		}
	}
	return candidates, nil
}

func (c *Client) fileHash(ctx context.Context, media domain.Media) (FileHash, error) {
	const algorithm = "opensubtitles"
	if c.hashCache != nil {
		value, byteSize, found, err := c.hashCache.GetMediaHash(ctx, media, algorithm)
		if err != nil {
			return FileHash{}, err
		}
		if found {
			return FileHash{MovieHash: value, ByteSize: byteSize}, nil
		}
	}

	hash, err := c.hasher.Hash(media.Fingerprint.Path)
	if err != nil {
		return FileHash{}, err
	}
	if c.hashCache != nil {
		if err := c.hashCache.PutMediaHash(ctx, media, algorithm, hash.MovieHash, hash.ByteSize); err != nil {
			return FileHash{}, err
		}
	}
	return hash, nil
}

func setBroadParameters(parameters url.Values, media domain.Media) {
	imdb := sanitizeIMDb(media.ExternalIDs.IMDb)
	if media.Ref.Kind == domain.MediaEpisode {
		if imdb != "" {
			parameters.Set("parent_imdb_id", imdb)
		} else if media.ExternalIDs.TMDB != 0 {
			parameters.Set("parent_tmdb_id", strconv.FormatInt(media.ExternalIDs.TMDB, 10))
		}
		if media.Season > 0 {
			parameters.Set("season_number", strconv.Itoa(media.Season))
		}
		if media.Episode > 0 {
			parameters.Set("episode_number", strconv.Itoa(media.Episode))
		}
	} else if imdb != "" {
		parameters.Set("imdb_id", imdb)
	} else if media.ExternalIDs.TMDB != 0 {
		parameters.Set("tmdb_id", strconv.FormatInt(media.ExternalIDs.TMDB, 10))
	}
	if imdb != "" || media.ExternalIDs.TMDB != 0 {
		return
	}
	if media.Title != "" {
		parameters.Set("query", media.Title)
	}
	// Arr stores the series premiere year, not the episode air year.
	if media.Ref.Kind != domain.MediaEpisode && media.Year > 0 {
		parameters.Set("year", strconv.Itoa(media.Year))
	}
}

func sanitizeIMDb(raw string) string {
	value := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "tt")
	value = strings.TrimLeft(value, "0")
	if value == "" && raw != "" {
		return "0"
	}
	return value
}

func openSubtitlesLanguage(language domain.Language) string {
	switch language.String() {
	case "pt":
		return "pt-PT"
	case "zh":
		return "zh-CN"
	case "es-MX":
		return "ea"
	default:
		return language.String()
	}
}

func fromOpenSubtitlesLanguage(raw string) (domain.Language, error) {
	if strings.EqualFold(strings.TrimSpace(raw), "ea") {
		return domain.ParseLanguage("es-MX")
	}
	language, err := domain.ParseLanguage(raw)
	if err != nil {
		return "", err
	}
	switch language.String() {
	case "pt-PT":
		return domain.Language("pt"), nil
	case "zh-CN":
		return domain.Language("zh"), nil
	default:
		return language, nil
	}
}

type loginResponse struct {
	BaseURL   string `json:"base_url"`
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expires_in"`
}

func (c *Client) login(ctx context.Context, force bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && c.expiresAt.After(c.clock.Now().Add(time.Minute)) {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{"username": c.config.Username, "password": c.config.Password})
	request, err := c.newRequest(ctx, http.MethodPost, "/login", bytes.NewReader(payload), false)
	if err != nil {
		return err
	}
	response, err := c.transport.Do(ctx, baseprovider.OperationAuth, request)
	if err != nil {
		if _, rejected := err.(*baseprovider.AuthenticationError); rejected {
			if persistErr := c.transport.DisableAuthentication(ctx, "login credentials rejected"); persistErr != nil {
				if response != nil {
					response.Body.Close()
				}
				return persistErr
			}
		}
		if response != nil {
			response.Body.Close()
		}
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("OpenSubtitles login returned HTTP %d", response.StatusCode)
	}
	var decoded loginResponse
	if err := decodeLimitedJSON(response.Body, &decoded); err != nil {
		return err
	}
	if decoded.Token == "" {
		return &baseprovider.InvalidPayloadError{Message: "login response has no token"}
	}
	expires := time.Duration(decoded.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = 12 * time.Hour
	}
	base, err := c.returnedAPIBase(decoded.BaseURL)
	if err != nil {
		return err
	}
	c.apiBaseURL = base
	c.token = decoded.Token
	c.expiresAt = c.clock.Now().Add(expires)
	return nil
}

func (c *Client) getJSONWithRefresh(ctx context.Context, path string, destination any) error {
	return c.getJSON(ctx, path, destination, true)
}

func (c *Client) getJSON(ctx context.Context, path string, destination any, allowRefresh bool) error {
	if err := c.login(ctx, false); err != nil {
		return err
	}
	request, err := c.newRequest(ctx, http.MethodGet, path, nil, true)
	if err != nil {
		return err
	}
	response, err := c.transport.Do(ctx, baseprovider.OperationSearch, request)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		if _, unauthorized := err.(*baseprovider.AuthenticationError); unauthorized && allowRefresh {
			if loginErr := c.login(ctx, true); loginErr != nil {
				return loginErr
			}
			return c.getJSON(ctx, path, destination, false)
		}
		if _, unauthorized := err.(*baseprovider.AuthenticationError); unauthorized {
			if persistErr := c.transport.DisableAuthentication(ctx, "credentials rejected after token refresh"); persistErr != nil {
				return persistErr
			}
		}
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("OpenSubtitles search returned HTTP %d", response.StatusCode)
	}
	if err := decodeLimitedJSON(response.Body, destination); err != nil {
		return err
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader, authenticated bool) (*http.Request, error) {
	base := c.config.BaseURL
	token := ""
	if authenticated {
		c.mu.Lock()
		if c.apiBaseURL != "" {
			base = c.apiBaseURL
		}
		token = c.token
		c.mu.Unlock()
	}
	endpoint := strings.TrimRight(base, "/") + path
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("create OpenSubtitles request: %w", err)
	}
	request.Header.Set("Api-Key", c.config.APIKey)
	request.Header.Set("User-Agent", c.config.UserAgent)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request, nil
}

type searchResponse struct {
	TotalPages int          `json:"total_pages"`
	Data       []searchItem `json:"data"`
}

type searchItem struct {
	ID         string `json:"id"`
	Attributes struct {
		Language         string  `json:"language"`
		ForeignPartsOnly bool    `json:"foreign_parts_only"`
		HearingImpaired  bool    `json:"hearing_impaired"`
		Ratings          float64 `json:"ratings"`
		DownloadCount    int64   `json:"download_count"`
		Release          string  `json:"release"`
		MovieHashMatch   bool    `json:"moviehash_match"`
		FeatureDetails   struct {
			MovieName     string `json:"movie_name"`
			Title         string `json:"title"`
			Year          int    `json:"year"`
			SeasonNumber  int    `json:"season_number"`
			EpisodeNumber int    `json:"episode_number"`
			IMDbID        int64  `json:"imdb_id"`
			TMDBID        int64  `json:"tmdb_id"`
			ParentIMDbID  int64  `json:"parent_imdb_id"`
			ParentTMDBID  int64  `json:"parent_tmdb_id"`
		} `json:"feature_details"`
		Files []struct {
			FileID   int64  `json:"file_id"`
			FileName string `json:"file_name"`
		} `json:"files"`
	} `json:"attributes"`
}

func normalizeCandidates(providerID string, query baseprovider.SearchQuery, items []searchItem) []domain.Candidate {
	var candidates []domain.Candidate
	for _, item := range items {
		language, err := fromOpenSubtitlesLanguage(item.Attributes.Language)
		if err != nil {
			continue
		}
		title := item.Attributes.FeatureDetails.MovieName
		if title == "" {
			title = item.Attributes.FeatureDetails.Title
		}
		for _, file := range item.Attributes.Files {
			if file.FileID <= 0 {
				continue
			}
			releases := []string{item.Attributes.Release}
			if file.FileName != "" && file.FileName != item.Attributes.Release {
				releases = append(releases, file.FileName)
			}
			externalIDs := domain.ExternalIDs{}
			if query.Media.Ref.Kind == domain.MediaEpisode {
				externalIDs.TMDB = item.Attributes.FeatureDetails.ParentTMDBID
				if item.Attributes.FeatureDetails.ParentIMDbID != 0 {
					externalIDs.IMDb = fmt.Sprintf("tt%07d", item.Attributes.FeatureDetails.ParentIMDbID)
				}
			} else {
				externalIDs.TMDB = item.Attributes.FeatureDetails.TMDBID
				if item.Attributes.FeatureDetails.IMDbID != 0 {
					externalIDs.IMDb = fmt.Sprintf("tt%07d", item.Attributes.FeatureDetails.IMDbID)
				}
			}
			candidate := domain.Candidate{ProviderID: providerID, ResultID: strconv.FormatInt(file.FileID, 10), Language: language, Kind: query.Media.Ref.Kind, Title: title, Year: item.Attributes.FeatureDetails.Year, Season: item.Attributes.FeatureDetails.SeasonNumber, Episode: item.Attributes.FeatureDetails.EpisodeNumber, ExternalIDs: externalIDs, ReleaseNames: releases, ExactHash: item.Attributes.MovieHashMatch, Forced: item.Attributes.ForeignPartsOnly, HearingImpaired: item.Attributes.HearingImpaired, Rating: min(max(item.Attributes.Ratings/10, 0), 1), Popularity: baseprovider.NormalizePopularity(item.Attributes.DownloadCount), DownloadCount: item.Attributes.DownloadCount, DownloadRef: strconv.FormatInt(file.FileID, 10), DownloadVersion: "srt-v1"}
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

type downloadResponse struct {
	Link         string `json:"link"`
	FileName     string `json:"file_name"`
	Message      string `json:"message"`
	ResetTimeUTC string `json:"reset_time_utc"`
}

func (c *Client) Download(ctx context.Context, candidate domain.Candidate, writer io.Writer) (baseprovider.DownloadMetadata, error) {
	fileID, err := strconv.ParseInt(candidate.DownloadRef, 10, 64)
	if err != nil || fileID <= 0 {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("OpenSubtitles candidate has invalid file ID")
	}
	decoded, err := c.requestDownloadLink(ctx, fileID, true)
	if err != nil {
		return baseprovider.DownloadMetadata{}, err
	}
	link, err := url.Parse(decoded.Link)
	if err != nil || link.User != nil || link.Host == "" || (link.Scheme != "https" && !(link.Scheme == "http" && sameOrigin(c.config.BaseURL, decoded.Link))) {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("OpenSubtitles returned an unsafe download link")
	}
	downloadRequest, err := c.newAbsoluteRequest(ctx, http.MethodGet, decoded.Link)
	if err != nil {
		return baseprovider.DownloadMetadata{}, err
	}
	download, err := c.transport.Do(ctx, baseprovider.OperationDownloadTransfer, downloadRequest)
	if err != nil {
		if download != nil {
			download.Body.Close()
		}
		return baseprovider.DownloadMetadata{}, err
	}
	defer download.Body.Close()
	if download.StatusCode < 200 || download.StatusCode >= 300 {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("OpenSubtitles temporary download returned HTTP %d", download.StatusCode)
	}
	written, err := io.Copy(writer, io.LimitReader(download.Body, c.config.MaxDownloadBytes+1))
	if err != nil {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("stream OpenSubtitles subtitle: %w", err)
	}
	if written > c.config.MaxDownloadBytes {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("OpenSubtitles subtitle exceeds %d bytes", c.config.MaxDownloadBytes)
	}
	return baseprovider.DownloadMetadata{Filename: decoded.FileName, ContentType: download.Header.Get("Content-Type")}, nil
}

func (c *Client) requestDownloadLink(ctx context.Context, fileID int64, allowRefresh bool) (downloadResponse, error) {
	if err := c.login(ctx, false); err != nil {
		return downloadResponse{}, err
	}
	payload, _ := json.Marshal(struct {
		FileID int64  `json:"file_id"`
		Format string `json:"sub_format"`
	}{FileID: fileID, Format: "srt"})
	request, err := c.newRequest(ctx, http.MethodPost, "/download", bytes.NewReader(payload), true)
	if err != nil {
		return downloadResponse{}, err
	}
	response, err := c.transport.Do(ctx, baseprovider.OperationDownload, request)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		if _, unauthorized := err.(*baseprovider.AuthenticationError); unauthorized && allowRefresh {
			if loginErr := c.login(ctx, true); loginErr != nil {
				return downloadResponse{}, loginErr
			}
			return c.requestDownloadLink(ctx, fileID, false)
		}
		if _, unauthorized := err.(*baseprovider.AuthenticationError); unauthorized {
			if persistErr := c.transport.DisableAuthentication(ctx, "credentials rejected after token refresh"); persistErr != nil {
				return downloadResponse{}, persistErr
			}
		}
		return downloadResponse{}, err
	}
	defer response.Body.Close()
	var decoded downloadResponse
	if response.StatusCode == http.StatusNotAcceptable {
		_ = decodeLimitedJSON(response.Body, &decoded)
		resetAt, _ := time.Parse(time.RFC3339, decoded.ResetTimeUTC)
		now := c.clock.Now()
		if window, found := baseprovider.ParseRateLimit(now, response.Header); found && window.Remaining <= 0 && window.ResetAt.After(now) {
			resetAt = window.ResetAt
		}
		if !resetAt.After(now) {
			resetAt = baseprovider.FallbackReset(now, "opensubtitles", baseprovider.CooldownDownloadQuota)
		}
		if err := c.transport.PersistCooldown(ctx, baseprovider.OperationDownload, baseprovider.CooldownDownloadQuota, resetAt); err != nil {
			return downloadResponse{}, err
		}
		return downloadResponse{}, &baseprovider.QuotaError{Scope: baseprovider.OperationDownload, ResetAt: resetAt, Message: decoded.Message}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return downloadResponse{}, fmt.Errorf("OpenSubtitles download request returned HTTP %d", response.StatusCode)
	}
	if err := decodeLimitedJSON(response.Body, &decoded); err != nil {
		return downloadResponse{}, err
	}
	if decoded.Link == "" {
		return downloadResponse{}, &baseprovider.InvalidPayloadError{Message: "download response has no valid link"}
	}
	return decoded, nil
}

func (c *Client) newAbsoluteRequest(ctx context.Context, method, endpoint string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create OpenSubtitles download request: %w", err)
	}
	request.Header.Set("User-Agent", c.config.UserAgent)
	return request, nil
}

func sameOrigin(first, second string) bool {
	a, errA := url.Parse(first)
	b, errB := url.Parse(second)
	return errA == nil && errB == nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func decodeLimitedJSON(reader io.Reader, destination any) error {
	return baseprovider.DecodeJSON(reader, destination, 4<<20, "OpenSubtitles response is not valid JSON")
}

// returnedAPIBase accepts only the documented public API hosts. An explicitly
// configured private endpoint stays private and may only return its own origin.
func (c *Client) returnedAPIBase(raw string) (string, error) {
	if raw == "" {
		return c.config.BaseURL, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	invalid := func() (string, error) {
		return "", &baseprovider.InvalidPayloadError{Message: "login returned an unsafe API host"}
	}
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return invalid()
	}
	configured, _ := url.Parse(c.config.BaseURL)
	trusted := func(host string) bool { return host == "api.opensubtitles.com" || host == "vip-api.opensubtitles.com" }
	if trusted(strings.ToLower(configured.Host)) {
		if u.Scheme != "https" || !trusted(strings.ToLower(u.Host)) || (u.Path != "" && u.Path != "/" && u.Path != "/api/v1" && u.Path != "/api/v1/") {
			return invalid()
		}
		return "https://" + strings.ToLower(u.Host) + "/api/v1", nil
	}
	if u.Scheme == "https" && trusted(strings.ToLower(u.Host)) && (u.Path == "" || u.Path == "/" || u.Path == "/api/v1" || u.Path == "/api/v1/") {
		return c.config.BaseURL, nil
	}
	if !sameOrigin(c.config.BaseURL, raw) {
		return invalid()
	}
	if u.Path != "" && u.Path != "/" && strings.TrimRight(u.Path, "/") != strings.TrimRight(configured.Path, "/") {
		return invalid()
	}
	return c.config.BaseURL, nil
}
