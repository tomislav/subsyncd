package subsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
	"subsyncd/internal/match"
	baseprovider "subsyncd/internal/provider"
)

const maxResponseBytes = 4 << 20

var subtitleIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

type Client struct {
	id        string
	config    Config
	transport baseprovider.Client
	clock     baseprovider.Clock
	titles    *ttlCache[[]titleEntry]
	listings  *ttlCache[[]subtitleRow]
}

func New(config Config, transport baseprovider.Client, clock baseprovider.Clock) (*Client, error) {
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = baseprovider.SystemClock{}
	}
	if transport.HTTP != nil {
		// Every SubSource endpoint answers directly; a redirect could carry the
		// key header to another origin.
		safe := *transport.HTTP
		safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		transport.HTTP = &safe
	}
	return &Client{
		id:        transport.ProviderID,
		config:    config,
		transport: transport,
		clock:     clock,
		titles:    newTTLCache[[]titleEntry](clock, catalogTTL, catalogLimit),
		listings:  newTTLCache[[]subtitleRow](clock, catalogTTL, catalogLimit),
	}, nil
}

func Factory(id string, node yaml.Node, dependencies baseprovider.Dependencies) (baseprovider.Provider, error) {
	payload, err := yaml.Marshal(&node)
	if err != nil {
		return nil, fmt.Errorf("encode SubSource config: %w", err)
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
		return nil, fmt.Errorf("SubSource HTTP client and provider gate are required")
	}
	dependencies.Gate.Configure(id, config.RequestsPerSecond, config.Burst, config.MaxConcurrent, "subsource")
	transport := baseprovider.Client{HTTP: dependencies.HTTPClient, Gate: dependencies.Gate, Clock: clock, ProviderID: id, ProviderType: "subsource"}
	return New(config, transport, clock)
}

func (c *Client) ID() string { return c.id }

func (c *Client) CheckSearchAvailability(ctx context.Context) error {
	return c.transport.CheckSearchAvailability(ctx)
}

func (c *Client) CheckDownloadAvailability(ctx context.Context) error {
	return c.transport.CheckDownloadAvailability(ctx)
}

func (c *Client) SearchCacheVersion() string { return "subsource-v1" }

func (c *Client) CanReuseCachedCandidate(candidate domain.Candidate) bool {
	return candidate.EvidenceVersion == evidenceVersion
}

func (c *Client) Capabilities() baseprovider.Capabilities {
	return baseprovider.Capabilities{SeasonPacks: true}
}

func (c *Client) SupportsLanguage(language domain.Language) bool {
	return len(languageSlugs(language)) > 0
}

func (c *Client) Search(ctx context.Context, query baseprovider.SearchQuery) ([]domain.Candidate, error) {
	if query.Mode != baseprovider.SearchBroad {
		return nil, fmt.Errorf("SubSource supports broad search only")
	}
	slugs := languageSlugs(query.Language)
	if len(slugs) == 0 {
		return nil, fmt.Errorf("SubSource does not support language %s", query.Language)
	}
	entry, found, err := c.resolve(ctx, query.Media)
	if err != nil || !found {
		return nil, err
	}
	var rows []subtitleRow
	for _, slug := range slugs {
		listed, err := c.list(ctx, entry.MovieID, slug)
		if err != nil {
			return nil, err
		}
		rows = append(rows, listed...)
	}
	return c.normalize(query, entry, rows), nil
}

// resolve finds the SubSource title (one season, for episodes) for the media.
func (c *Client) resolve(ctx context.Context, media domain.Media) (titleEntry, bool, error) {
	episode := media.Ref.Kind == domain.MediaEpisode
	parameters := url.Values{"type": {"movie"}}
	if episode {
		parameters.Set("type", "series")
	}
	imdb := strings.ToLower(strings.TrimSpace(media.ExternalIDs.IMDb))
	switch {
	case imdb != "":
		parameters.Set("searchType", "imdb")
		parameters.Set("imdb", imdb)
	case strings.TrimSpace(media.Title) != "":
		parameters.Set("searchType", "text")
		parameters.Set("q", strings.TrimSpace(media.Title))
		if !episode && media.Year > 0 {
			parameters.Set("year", strconv.Itoa(media.Year))
		}
	default:
		return titleEntry{}, false, nil
	}
	entries, err := c.searchTitles(ctx, parameters)
	if err != nil {
		return titleEntry{}, false, err
	}
	var matches []titleEntry
	for _, entry := range entries {
		if entry.MovieID <= 0 || (entry.Type == "movie") == episode {
			continue
		}
		if episode && (entry.Season == nil || *entry.Season != media.Season) {
			continue
		}
		entryIMDb := strings.ToLower(strings.TrimSpace(entry.IMDb))
		if imdb != "" {
			if entryIMDb != imdb {
				continue
			}
		} else if match.NormalizeIdentity(entry.Title) != match.NormalizeIdentity(media.Title) || !episode && media.Year > 0 && entry.ReleaseYear != media.Year {
			continue
		}
		matches = append(matches, entry)
	}
	if len(matches) == 0 {
		return titleEntry{}, false, nil
	}
	for _, other := range matches[1:] {
		if !strings.EqualFold(other.IMDb, matches[0].IMDb) || other.IMDb == "" {
			return titleEntry{}, false, nil
		}
	}
	return matches[0], true, nil
}

func (c *Client) searchTitles(ctx context.Context, parameters url.Values) ([]titleEntry, error) {
	return c.titles.load(ctx, parameters.Encode(), func() ([]titleEntry, bool, error) {
		var decoded struct {
			Success *bool        `json:"success"`
			Data    []titleEntry `json:"data"`
		}
		found, err := c.getJSON(ctx, "/movies/search", parameters, &decoded)
		if err != nil || !found {
			return nil, false, err
		}
		if decoded.Success == nil || !*decoded.Success {
			return nil, false, &baseprovider.InvalidPayloadError{Message: "SubSource title search was not successful"}
		}
		return decoded.Data, true, nil
	})
}

// list reads every page of one title's subtitles in one language. A 404 is an
// empty answer but is not cached, since it may come from a transient gateway
// fault rather than a missing title.
func (c *Client) list(ctx context.Context, movieID int64, slug string) ([]subtitleRow, error) {
	return c.listings.load(ctx, strconv.FormatInt(movieID, 10)+"\x00"+slug, func() ([]subtitleRow, bool, error) {
		var rows []subtitleRow
		for page := 1; page <= c.config.MaxPages; page++ {
			var decoded struct {
				Success    *bool         `json:"success"`
				Data       []subtitleRow `json:"data"`
				Pagination struct {
					Pages int `json:"pages"`
				} `json:"pagination"`
			}
			parameters := url.Values{"movieId": {strconv.FormatInt(movieID, 10)}, "language": {slug}, "limit": {"100"}, "page": {strconv.Itoa(page)}, "sort": {"popular"}}
			found, err := c.getJSON(ctx, "/subtitles", parameters, &decoded)
			if err != nil {
				return nil, false, err
			}
			if !found {
				return rows, false, nil
			}
			if decoded.Success == nil || !*decoded.Success {
				return nil, false, &baseprovider.InvalidPayloadError{Message: "SubSource subtitle listing was not successful"}
			}
			rows = append(rows, decoded.Data...)
			if page >= decoded.Pagination.Pages {
				break
			}
		}
		return rows, true, nil
	})
}

// getJSON reports found=false for 404 so a missing title or listing is an
// empty answer rather than a failure.
func (c *Client) getJSON(ctx context.Context, path string, parameters url.Values, destination any) (bool, error) {
	request, err := c.newRequest(ctx, path+"?"+parameters.Encode())
	if err != nil {
		return false, fmt.Errorf("create SubSource search request")
	}
	response, err := c.do(ctx, baseprovider.OperationSearch, request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusNotFound:
		return false, nil
	case response.StatusCode != http.StatusOK:
		return false, fmt.Errorf("SubSource search returned HTTP %d", response.StatusCode)
	}
	if err := baseprovider.DecodeJSON(response.Body, destination, maxResponseBytes, "SubSource search response is not valid JSON"); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) newRequest(ctx context.Context, pathAndQuery string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.config.BaseURL, "/")+pathAndQuery, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-API-Key", c.config.APIKey)
	request.Header.Set("Accept", "application/json")
	return request, nil
}

// do runs a request through the shared transport and adds SubSource's
// authentication and quota handling. On error the response body is closed.
func (c *Client) do(ctx context.Context, operation baseprovider.Operation, request *http.Request) (*http.Response, error) {
	response, err := c.transport.Do(ctx, operation, request)
	if err == nil {
		return response, nil
	}
	if response == nil {
		return nil, err
	}
	defer response.Body.Close()
	var authentication *baseprovider.AuthenticationError
	if errors.As(err, &authentication) {
		if disableErr := c.transport.DisableAuthentication(ctx, "SubSource API key rejected (HTTP 401)"); disableErr != nil {
			return nil, fmt.Errorf("disable SubSource provider: %w", disableErr)
		}
		return nil, &baseprovider.AuthenticationError{Message: "API key rejected"}
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return nil, c.quotaLimit(ctx, response.Header, err)
	}
	return nil, err
}

// quotaLimit recognizes the hourly and daily caps, which SubSource enforces
// without reporting them. A 429 that does not report an exhausted minute
// window, whether its headers show spare capacity or are absent, must come
// from one of those, so the whole key cools down. Local pacing keeps requests
// well under the minute limit, so an unexplained 429 is not a minute overrun.
func (c *Client) quotaLimit(ctx context.Context, headers http.Header, fallback error) error {
	var cooldown *baseprovider.CooldownError
	if !errors.As(fallback, &cooldown) || strings.TrimSpace(headers.Get("Retry-After")) != "" {
		return fallback
	}
	if remaining, err := strconv.ParseInt(strings.TrimSpace(headers.Get("X-RateLimit-Remaining")), 10, 64); err == nil && remaining <= 0 {
		return fallback
	}
	reset, err := c.transport.PersistCooldown(ctx, baseprovider.OperationAll, baseprovider.CooldownDownloadQuota, time.Time{})
	if err != nil {
		return err
	}
	return &baseprovider.QuotaError{Scope: baseprovider.OperationAll, ResetAt: reset, Message: "SubSource hourly or daily limit"}
}

// Download rebuilds the fixed download path from the numeric subtitle ID. The
// archive's server-supplied name embeds media metadata and is never used.
func (c *Client) Download(ctx context.Context, candidate domain.Candidate, writer io.Writer) (baseprovider.DownloadMetadata, error) {
	if candidate.ProviderID != c.id || !subtitleIDPattern.MatchString(candidate.ResultID) {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("invalid SubSource subtitle identity")
	}
	request, err := c.newRequest(ctx, "/subtitles/"+candidate.ResultID+"/download")
	if err != nil {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("create SubSource download request")
	}
	request.Header.Set("Accept", "application/zip")
	response, err := c.do(ctx, baseprovider.OperationDownload, request)
	if err != nil {
		return baseprovider.DownloadMetadata{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("SubSource download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > c.config.MaxDownloadBytes {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("SubSource download exceeds %d bytes", c.config.MaxDownloadBytes)
	}
	written, err := io.Copy(writer, io.LimitReader(response.Body, c.config.MaxDownloadBytes+1))
	if err != nil {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("stream SubSource download: %w", err)
	}
	if written > c.config.MaxDownloadBytes {
		return baseprovider.DownloadMetadata{}, fmt.Errorf("SubSource download exceeds %d bytes", c.config.MaxDownloadBytes)
	}
	return baseprovider.DownloadMetadata{Filename: "subsource-" + candidate.ResultID + ".zip", ContentType: response.Header.Get("Content-Type")}, nil
}
