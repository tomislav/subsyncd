package titlovi

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
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
	baseprovider "subsyncd/internal/provider"
)

type Client struct {
	id        string
	config    Config
	transport baseprovider.Client
	clock     baseprovider.Clock
	mu        sync.Mutex
	token     string
	userID    int64
	expiresAt time.Time
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
		return nil, fmt.Errorf("encode Titlovi config: %w", err)
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
		return nil, fmt.Errorf("Titlovi HTTP client and provider gate are required")
	}
	dependencies.Gate.Configure(id, config.RequestsPerSecond, config.Burst, config.MaxConcurrent, "titlovi")
	transport := baseprovider.Client{HTTP: dependencies.HTTPClient, Gate: dependencies.Gate, Clock: clock, ProviderID: id, ProviderType: "titlovi"}
	return New(config, transport, clock)
}

func (c *Client) ID() string { return c.id }

func (c *Client) Capabilities() baseprovider.Capabilities {
	return baseprovider.Capabilities{SeasonPacks: true}
}

func (c *Client) SupportsLanguage(language domain.Language) bool {
	_, ok := titloviLanguage(language)
	return ok
}

type loginResponse struct {
	Token          string `json:"Token"`
	UserID         int64  `json:"UserId"`
	ExpirationDate string `json:"ExpirationDate"`
}

func (c *Client) login(ctx context.Context, force bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && c.expiresAt.After(c.clock.Now().Add(time.Minute)) {
		return nil
	}
	parameters := url.Values{"username": {c.config.Username}, "password": {c.config.Password}, "json": {"true"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.config.APIBaseURL, "/")+"/gettoken?"+parameters.Encode(), nil)
	if err != nil {
		return fmt.Errorf("create Titlovi login request: %w", err)
	}
	response, err := c.transport.Do(ctx, baseprovider.OperationAuth, request)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Titlovi login returned HTTP %d", response.StatusCode)
	}
	var decoded loginResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded); err != nil || decoded.Token == "" || decoded.UserID <= 0 {
		return &baseprovider.InvalidPayloadError{Message: "Titlovi login response is incomplete"}
	}
	expiresAt, err := parseExpiration(decoded.ExpirationDate)
	if err != nil {
		return &baseprovider.InvalidPayloadError{Message: "Titlovi token expiration is invalid"}
	}
	c.token = decoded.Token
	c.userID = decoded.UserID
	c.expiresAt = expiresAt
	return nil
}

func parseExpiration(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	location, err := time.LoadLocation("Europe/Zagreb")
	if err != nil {
		return time.Time{}, err
	}
	return time.ParseInLocation("2006-01-02T15:04:05.999999999", value, location)
}

func (c *Client) Search(ctx context.Context, query baseprovider.SearchQuery) ([]domain.Candidate, error) {
	if query.Mode != baseprovider.SearchBroad {
		return nil, fmt.Errorf("Titlovi supports broad search only")
	}
	language, ok := titloviLanguage(query.Language)
	if !ok {
		return nil, fmt.Errorf("Titlovi does not support language %s", query.Language)
	}
	parameters := url.Values{"query": {query.Media.Title}, "lang": {language}, "json": {"true"}}
	if query.Media.Year > 0 {
		parameters.Set("year", strconv.Itoa(query.Media.Year))
	}
	if query.Media.ExternalIDs.IMDb != "" {
		parameters.Set("imdbID", query.Media.ExternalIDs.IMDb)
	}
	if query.Media.Ref.Kind == domain.MediaEpisode {
		parameters.Set("season", strconv.Itoa(query.Media.Season))
		parameters.Set("episode", strconv.Itoa(query.Media.Episode))
	}
	return c.search(ctx, query, parameters, true)
}

func (c *Client) search(ctx context.Context, query baseprovider.SearchQuery, parameters url.Values, allowRefresh bool) ([]domain.Candidate, error) {
	if err := c.login(ctx, false); err != nil {
		return nil, err
	}
	c.mu.Lock()
	parameters.Set("token", c.token)
	parameters.Set("userid", strconv.FormatInt(c.userID, 10))
	c.mu.Unlock()
	var candidates []domain.Candidate
	for page := 1; page <= c.config.MaxPages; page++ {
		if page > 1 {
			parameters.Set("pg", strconv.Itoa(page))
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.config.APIBaseURL, "/")+"/search?"+parameters.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("create Titlovi search request: %w", err)
		}
		response, err := c.transport.Do(ctx, baseprovider.OperationSearch, request)
		if err != nil {
			if response != nil {
				response.Body.Close()
			}
			if _, unauthorized := err.(*baseprovider.AuthenticationError); unauthorized && allowRefresh {
				if loginErr := c.login(ctx, true); loginErr != nil {
					return nil, loginErr
				}
				return c.search(ctx, query, parameters, false)
			}
			if _, unauthorized := err.(*baseprovider.AuthenticationError); unauthorized {
				_ = c.transport.DisableAuthentication(ctx, "credentials rejected after token refresh")
			}
			return nil, err
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			response.Body.Close()
			return nil, fmt.Errorf("Titlovi search returned HTTP %d", response.StatusCode)
		}
		var decoded searchResponse
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&decoded)
		response.Body.Close()
		if decodeErr != nil {
			return nil, &baseprovider.InvalidPayloadError{Message: "Titlovi search response is not valid JSON"}
		}
		candidates = append(candidates, c.normalize(query, decoded.SubtitleResults)...)
		if decoded.PagesAvailable <= page || len(decoded.SubtitleResults) == 0 {
			break
		}
	}
	return candidates, nil
}

type searchResponse struct {
	PagesAvailable  int          `json:"PagesAvailable"`
	SubtitleResults []searchItem `json:"SubtitleResults"`
}

type searchItem struct {
	ID            int64   `json:"Id"`
	Language      string  `json:"Lang"`
	Link          string  `json:"Link"`
	Release       string  `json:"Release"`
	Title         string  `json:"Title"`
	Season        int     `json:"Season"`
	Episode       int     `json:"Episode"`
	Year          int     `json:"Year"`
	Rating        float64 `json:"Rating"`
	DownloadCount int64   `json:"DownloadCount"`
}

var akaPattern = regexp.MustCompile(`(?i)\s+aka\s+`)

func (c *Client) normalize(query baseprovider.SearchQuery, items []searchItem) []domain.Candidate {
	var candidates []domain.Candidate
	for _, item := range items {
		language, err := fromTitloviLanguage(item.Language)
		if err != nil || !domain.EquivalentLanguage(language, query.Language) || item.ID <= 0 {
			continue
		}
		if query.Media.Ref.Kind == domain.MediaEpisode {
			if item.Season != query.Media.Season || (item.Episode != 0 && item.Episode != query.Media.Episode) {
				continue
			}
		}
		downloadRef, ok := c.downloadReference(item.Link)
		if !ok {
			continue
		}
		titles := akaPattern.Split(item.Title, 2)
		candidate := domain.Candidate{ProviderID: c.id, ResultID: strconv.FormatInt(item.ID, 10), Language: language, Kind: query.Media.Ref.Kind, Title: strings.TrimSpace(titles[0]), Year: item.Year, Season: item.Season, Episode: item.Episode, ExternalIDs: domain.ExternalIDs{IMDb: query.Media.ExternalIDs.IMDb}, ReleaseNames: []string{item.Release}, Rating: min(max(item.Rating/10, 0), 1), Popularity: baseprovider.NormalizePopularity(item.DownloadCount), DownloadCount: item.DownloadCount, DownloadRef: downloadRef}
		if query.Media.Ref.Kind == domain.MediaEpisode && item.Episode == 0 {
			candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: item.Season}
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func (c *Client) downloadReference(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if parsed.IsAbs() && !c.allowedDownloadURL(parsed) {
		return "", false
	}
	if !strings.HasPrefix(parsed.Path, "/") {
		parsed.Path = "/" + parsed.Path
	}
	return parsed.EscapedPath() + querySuffix(parsed.RawQuery), true
}

func querySuffix(raw string) string {
	if raw == "" {
		return ""
	}
	return "?" + raw
}

func (c *Client) Download(ctx context.Context, candidate domain.Candidate, writer io.Writer) (baseprovider.DownloadMetadata, error) {
	return c.download(ctx, candidate, writer, true)
}

func (c *Client) download(ctx context.Context, candidate domain.Candidate, writer io.Writer, allowRefresh bool) (baseprovider.DownloadMetadata, error) {
	if err := c.login(ctx, false); err != nil {
		return baseprovider.DownloadMetadata{}, err
	}
	base, _ := url.Parse(strings.TrimRight(c.config.DownloadBaseURL, "/") + "/")
	reference, err := url.Parse(candidate.DownloadRef)
	if err != nil {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("Titlovi candidate has invalid download reference")
	}
	endpoint := base.ResolveReference(reference)
	if !c.allowedDownloadURL(endpoint) {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("Titlovi download URL is not allowed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("create Titlovi download request: %w", err)
	}
	safeHTTP := *c.transport.HTTP
	safeHTTP.CheckRedirect = func(request *http.Request, _ []*http.Request) error {
		if !c.allowedDownloadURL(request.URL) {
			return fmt.Errorf("Titlovi redirect target is not allowed")
		}
		return nil
	}
	transport := c.transport
	transport.HTTP = &safeHTTP
	response, err := transport.Do(ctx, baseprovider.OperationDownload, request)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		if _, unauthorized := err.(*baseprovider.AuthenticationError); unauthorized && allowRefresh {
			if loginErr := c.login(ctx, true); loginErr != nil {
				return baseprovider.DownloadMetadata{}, loginErr
			}
			return c.download(ctx, candidate, writer, false)
		}
		if _, unauthorized := err.(*baseprovider.AuthenticationError); unauthorized {
			_ = c.transport.DisableAuthentication(ctx, "credentials rejected after token refresh")
		}
		return baseprovider.DownloadMetadata{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("Titlovi download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > c.config.MaxDownloadBytes {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("Titlovi download exceeds %d bytes", c.config.MaxDownloadBytes)
	}
	written, err := io.Copy(writer, io.LimitReader(response.Body, c.config.MaxDownloadBytes+1))
	if err != nil {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("stream Titlovi download: %w", err)
	}
	if written > c.config.MaxDownloadBytes {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("Titlovi download exceeds %d bytes", c.config.MaxDownloadBytes)
	}
	return baseprovider.DownloadMetadata{Filename: path.Base(endpoint.Path), ContentType: response.Header.Get("Content-Type")}, nil
}

func (c *Client) allowedDownloadURL(endpoint *url.URL) bool {
	if endpoint == nil || (endpoint.Scheme != "https" && !(c.config.AllowInsecureForTests && endpoint.Scheme == "http")) {
		return false
	}
	for _, host := range c.config.DownloadHosts {
		if strings.EqualFold(endpoint.Host, host) || strings.EqualFold(endpoint.Hostname(), host) {
			return true
		}
	}
	return false
}
