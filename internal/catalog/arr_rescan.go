package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"subsyncd/internal/domain"
)

const commandPath = "/api/v3/command"

// ArrRescanClient asks one Arr instance to rescan a series (Sonarr) or a
// movie (Radarr) from disk, so it sees subtitle sidecars subsyncd added,
// replaced or removed without waiting for its scheduled refresh. It sends
// RescanSeries or RescanMovie, never a Refresh command, which would also query
// Arr's metadata providers.
type ArrRescanClient struct {
	client *arrClient
	kind   domain.MediaKind
}

// NewArrRescanClient builds the client for an instance of instanceType
// (sonarr or radarr). A nil httpClient uses the Arr client's default.
func NewArrRescanClient(instance, instanceType, rawURL, apiKey string, httpClient *http.Client) (*ArrRescanClient, error) {
	kind := domain.MediaEpisode
	switch instanceType {
	case "sonarr":
	case "radarr":
		kind = domain.MediaMovie
	default:
		return nil, fmt.Errorf("Arr instance %q has unsupported type", instance)
	}
	client, err := newArrClient(instance, rawURL, apiKey, httpClient)
	if err != nil {
		return nil, err
	}
	return &ArrRescanClient{client: client, kind: kind}, nil
}

// Kind is the media kind the instance manages.
func (c *ArrRescanClient) Kind() domain.MediaKind { return c.kind }

// Rescan queues the rescan command for a Sonarr series ID or a Radarr movie
// ID. Any 2xx answer (Arr replies 201 with the queued command) counts as
// delivered; the response body is discarded.
func (c *ArrRescanClient) Rescan(ctx context.Context, id int64) error {
	if id <= 0 {
		return &arrAPIError{Instance: c.client.instance, Operation: "command_rescan", Kind: "invalid_request"}
	}
	var body any = struct {
		Name     string `json:"name"`
		SeriesID int64  `json:"seriesId"`
	}{Name: "RescanSeries", SeriesID: id}
	if c.kind == domain.MediaMovie {
		body = struct {
			Name    string `json:"name"`
			MovieID int64  `json:"movieId"`
		}{Name: "RescanMovie", MovieID: id}
	}
	return c.client.sendJSON(ctx, http.MethodPost, commandPath, body, nil, "command_rescan")
}

// ArrRequestFailure classifies an error from an Arr request: status is Arr's
// HTTP status (0 when no answer arrived or the request was never sent) and
// retryable whether the same request may succeed later (no answer, a
// timeout, or a 5xx). Errors that are not Arr request errors report 0, false.
func ArrRequestFailure(err error) (status int, retryable bool) {
	var validation *arrValidationError
	if errors.As(err, &validation) {
		return validation.StatusCode, false
	}
	var api *arrAPIError
	if errors.As(err, &api) {
		return api.StatusCode, api.Retryable
	}
	return 0, false
}
