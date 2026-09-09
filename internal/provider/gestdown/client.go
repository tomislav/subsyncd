package gestdown

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
	"subsyncd/internal/domain"
	base "subsyncd/internal/provider"
)

type Client struct {
	id        string
	config    Config
	transport base.Client
}

func New(config Config, transport base.Client) (*Client, error) {
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if transport.HTTP == nil || transport.Gate == nil {
		return nil, fmt.Errorf("Gestdown HTTP client and provider gate are required")
	}
	if transport.Clock == nil {
		transport.Clock = base.SystemClock{}
	}
	safe := *transport.HTTP
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport.HTTP = &safe
	return &Client{id: transport.ProviderID, config: config, transport: transport}, nil
}
func Factory(id string, node yaml.Node, deps base.Dependencies) (base.Provider, error) {
	payload, err := yaml.Marshal(&node)
	if err != nil {
		return nil, fmt.Errorf("invalid Gestdown configuration")
	}
	config, err := DecodeConfig(payload)
	if err != nil {
		return nil, err
	}
	if deps.Gate == nil || deps.HTTPClient == nil {
		return nil, fmt.Errorf("Gestdown HTTP client and provider gate are required")
	}
	deps.Gate.Configure(id, config.RequestsPerSecond, config.Burst, config.MaxConcurrent, "gestdown")
	return New(config, base.Client{HTTP: deps.HTTPClient, Gate: deps.Gate, Clock: deps.Clock, ProviderID: id, ProviderType: "gestdown"})
}
func (c *Client) ID() string                      { return c.id }
func (c *Client) Capabilities() base.Capabilities { return base.Capabilities{} }
func (c *Client) SupportsLanguage(language domain.Language) bool {
	_, ok := languageNames[language]
	return ok
}

const uuidPattern = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

var showIDPattern = regexp.MustCompile(`^` + uuidPattern + `$`)

// Whole-season IDs are deliberately excluded from the episode adapter. Only
// explicitly extracted episode IDs from the current API contract are accepted.
var subtitleIDPattern = regexp.MustCompile(`^(?:` + uuidPattern + `|sp_` + uuidPattern + `_ep_[1-9][0-9]*)$`)

type show struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	TVDB int64  `json:"tvDbId"`
	TMDB int64  `json:"tmdbId"`
}
type showResponse struct {
	Shows []show `json:"shows"`
}
type episode struct {
	Season *int   `json:"season"`
	Number *int   `json:"number"`
	Show   string `json:"show"`
}
type subtitle struct {
	ID              string `json:"subtitleId"`
	Version         string `json:"version"`
	Release         string `json:"release"`
	Language        string `json:"language"`
	Completed       bool   `json:"completed"`
	HearingImpaired *bool  `json:"hearingImpaired"`
	DownloadCount   int64  `json:"downloadCount"`
}
type episodeResponse struct {
	Episode   *episode   `json:"episode"`
	Subtitles []subtitle `json:"matchingSubtitles"`
}

func (c *Client) Search(ctx context.Context, q base.SearchQuery) ([]domain.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q.Media.Ref.Kind != domain.MediaEpisode {
		return nil, nil
	}
	if q.Mode != base.SearchBroad {
		return nil, fmt.Errorf("Gestdown supports broad search only")
	}
	if !c.SupportsLanguage(q.Language) {
		return nil, fmt.Errorf("unsupported Gestdown language")
	}
	if q.Media.Season < 0 || q.Media.Episode <= 0 {
		return nil, nil
	}
	var shows showResponse
	if q.Media.ExternalIDs.TVDB > 0 {
		if _, err := c.getJSON(ctx, "/shows/external/tvdb/"+strconv.FormatInt(q.Media.ExternalIDs.TVDB, 10), &shows); err != nil {
			return nil, err
		}
	}
	if len(shows.Shows) == 0 && len([]rune(strings.TrimSpace(q.Media.Title))) >= 3 {
		if _, err := c.getJSON(ctx, "/shows/search/"+url.PathEscape(q.Media.Title), &shows); err != nil {
			return nil, err
		}
	}
	var candidates []domain.Candidate
	seen := map[string]bool{}
	for _, s := range shows.Shows {
		if !showIDPattern.MatchString(s.ID) || seen[s.ID] || !matchesShow(q.Media, s) {
			continue
		}
		seen[s.ID] = true
		var result episodeResponse
		found, err := c.getJSON(ctx, fmt.Sprintf("/subtitles/get/%s/%d/%d/%s", s.ID, q.Media.Season, q.Media.Episode, q.Language), &result)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		ep := result.Episode
		if ep == nil || ep.Season == nil || ep.Number == nil || *ep.Season != q.Media.Season || *ep.Number != q.Media.Episode || titleKey(ep.Show) == "" || titleKey(ep.Show) != titleKey(s.Name) {
			continue
		}
		for _, item := range result.Subtitles {
			language, ok := returnedLanguage(item.Language)
			if !ok || language != q.Language || !item.Completed || item.HearingImpaired == nil || !subtitleIDPattern.MatchString(item.ID) {
				continue
			}
			if strings.HasPrefix(item.ID, "sp_") && !strings.HasSuffix(item.ID, "_ep_"+strconv.Itoa(*ep.Number)) {
				continue
			}
			var releases []string
			for _, raw := range []string{item.Release, item.Version} {
				if v := strings.TrimSpace(raw); v != "" && (len(releases) == 0 || v != releases[0]) {
					releases = append(releases, v)
				}
			}
			candidates = append(candidates, domain.Candidate{ProviderID: c.id, ResultID: item.ID, Kind: domain.MediaEpisode, Title: s.Name, Season: *ep.Season, Episode: *ep.Number, ExternalIDs: domain.ExternalIDs{TVDB: s.TVDB, TMDB: s.TMDB}, Language: language, ReleaseNames: releases, HearingImpaired: *item.HearingImpaired, DownloadCount: max(item.DownloadCount, 0), Popularity: base.NormalizePopularity(item.DownloadCount), DownloadRef: "/subtitles/download/" + item.ID})
		}
	}
	return candidates, nil
}
func matchesShow(media domain.Media, s show) bool {
	if s.TVDB < 0 || s.TMDB < 0 || strings.TrimSpace(s.Name) == "" {
		return false
	}
	if media.ExternalIDs.TVDB > 0 && s.TVDB > 0 {
		return media.ExternalIDs.TVDB == s.TVDB
	}
	if media.ExternalIDs.TMDB > 0 && s.TMDB > 0 {
		return media.ExternalIDs.TMDB == s.TMDB
	}
	for _, title := range append([]string{media.Title}, media.AlternateTitles...) {
		if titleKey(title) != "" && titleKey(title) == titleKey(s.Name) {
			return true
		}
	}
	return false
}
func titleKey(raw string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, raw)
}

func (c *Client) request(ctx context.Context, operation base.Operation, path string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.config.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid Gestdown request")
	}
	request.Header.Set("User-Agent", "subsyncd")
	response, err := c.transport.Do(ctx, operation, request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return nil, err
	}
	if response.StatusCode == http.StatusLocked || response.StatusCode == http.StatusTooManyRequests {
		response.Body.Close()
		reset := c.transport.Clock.Now().Add(5 * time.Minute)
		if window, found := base.ParseRateLimit(c.transport.Clock.Now(), response.Header); found && window.Remaining <= 0 {
			reset = window.ResetAt
		}
		reason := base.CooldownServiceBusy
		if response.StatusCode == http.StatusTooManyRequests {
			reason = base.CooldownRateLimit
		}
		if err := c.transport.PersistCooldown(ctx, operation, reason, reset); err != nil {
			return nil, err
		}
		return nil, &base.CooldownError{ProviderID: c.id, Scope: operation, Reason: string(reason), ResetAt: reset}
	}
	return response, nil
}
func (c *Client) getJSON(ctx context.Context, path string, destination any) (bool, error) {
	response, err := c.request(ctx, base.OperationSearch, path)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("Gestdown search returned HTTP %d", response.StatusCode)
	}
	if err := base.DecodeJSON(response.Body, destination, 4<<20, "invalid Gestdown search response"); err != nil {
		return false, err
	}
	return true, nil
}
func (c *Client) Download(ctx context.Context, candidate domain.Candidate, writer io.Writer) (base.DownloadMetadata, error) {
	if candidate.ProviderID != c.id || !subtitleIDPattern.MatchString(candidate.ResultID) {
		return base.DownloadMetadata{}, fmt.Errorf("invalid Gestdown subtitle identity")
	}
	response, err := c.request(ctx, base.OperationDownload, "/subtitles/download/"+candidate.ResultID)
	if err != nil {
		return base.DownloadMetadata{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return base.DownloadMetadata{}, fmt.Errorf("Gestdown download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > c.config.MaxDownloadBytes {
		return base.DownloadMetadata{}, &base.InvalidPayloadError{Message: "Gestdown download exceeds size limit"}
	}
	n, err := io.Copy(writer, io.LimitReader(response.Body, c.config.MaxDownloadBytes+1))
	if err != nil {
		return base.DownloadMetadata{}, err
	}
	if n > c.config.MaxDownloadBytes {
		return base.DownloadMetadata{}, &base.InvalidPayloadError{Message: "Gestdown download exceeds size limit"}
	}
	return base.DownloadMetadata{Filename: "subtitle.srt", ContentType: response.Header.Get("Content-Type")}, nil
}
