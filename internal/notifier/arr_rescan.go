package notifier

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
)

// ArrRescanName is the notifier name, stored in notifications.notifier, of
// the rescans subsyncd asks Sonarr and Radarr for after it changes a sidecar.
const ArrRescanName = "arr_rescan"

// ArrRescanDelay is how long a queued rescan waits before it is due, so the
// changes of episodes installed in quick succession (a season pack, a burst
// of imports) coalesce into one rescan per series: a change for a series
// whose rescan is still queued adds none (RescanCoalesceKey). Silo
// notifications are due at once.
const ArrRescanDelay = 60 * time.Second

// Skip reasons reported for an Arr rescan that is not sent.
const (
	SkipRescanDisabled = "rescan_disabled"
	SkipUnknownArrID   = "unknown_arr_id"
)

// ArrRescanner sends the rescan command to one Arr instance.
type ArrRescanner interface {
	Kind() domain.MediaKind
	Rescan(ctx context.Context, id int64) error
}

// ArrRescan asks the Arr instance that owns a media file to rescan its series
// or movie from disk, so Sonarr and Radarr know about subtitle sidecars
// subsyncd added, replaced or removed before their scheduled refresh.
// Deliveries for one series or movie of one instance coalesce.
type ArrRescan struct {
	instances map[string]ArrRescanner
}

// NewArrRescan builds the notifier for the instances whose rescans are on.
func NewArrRescan(instances map[string]ArrRescanner) *ArrRescan {
	copied := make(map[string]ArrRescanner, len(instances))
	for name, rescanner := range instances {
		copied[name] = rescanner
	}
	return &ArrRescan{instances: copied}
}

// RescanID returns the Arr ID a rescan of media names: the Sonarr series ID
// of an episode or the Radarr movie ID of a movie. It reports false when the
// ID is unknown (a media row stored before subsyncd kept series IDs).
func RescanID(media domain.Media) (int64, bool) {
	switch media.Ref.Kind {
	case domain.MediaEpisode:
		return media.SeriesID, media.SeriesID > 0
	case domain.MediaMovie:
		return media.EntityID, media.EntityID > 0
	}
	return 0, false
}

// RescanCoalesceKey is the instance, the media kind and the series or movie
// ID: one rescan covers every sidecar changed for it, such as a season
// pack's episodes. It reports false when the ID is unknown.
func RescanCoalesceKey(media domain.Media) (string, bool) {
	id, ok := RescanID(media)
	if !ok {
		return "", false
	}
	return media.Ref.Instance + "\x00" + string(media.Ref.Kind) + "\x00" + strconv.FormatInt(id, 10), true
}

// CoalesceKey coalesces the deliveries of one leased batch on
// RescanCoalesceKey.
func (a *ArrRescan) CoalesceKey(media domain.Media, _ string) (string, error) {
	key, ok := RescanCoalesceKey(media)
	if !ok {
		return "", &SkippedError{Reason: SkipUnknownArrID}
	}
	return key, nil
}

// DeliveryAttrs names the instance and the series or movie.
func (a *ArrRescan) DeliveryAttrs(media domain.Media) []slog.Attr {
	attrs := []slog.Attr{slog.String("instance", observability.SafeText(media.Ref.Instance)), slog.String("media_kind", string(media.Ref.Kind))}
	if id, ok := RescanID(media); ok {
		key := "series_id"
		if media.Ref.Kind == domain.MediaMovie {
			key = "movie_id"
		}
		attrs = append(attrs, slog.Int64(key, id))
	}
	return attrs
}

func (a *ArrRescan) SubtitleChanged(ctx context.Context, media domain.Media, _ string) error {
	rescanner, ok := a.instances[media.Ref.Instance]
	if !ok {
		// Turned off, or the instance removed, after the intent was queued.
		return &SkippedError{Reason: SkipRescanDisabled}
	}
	id, ok := RescanID(media)
	if !ok {
		return &SkippedError{Reason: SkipUnknownArrID}
	}
	if rescanner.Kind() != media.Ref.Kind {
		return &DeliveryError{Target: "Arr rescan", Reason: "media kind does not match the instance"}
	}
	err := rescanner.Rescan(ctx, id)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	status, retryable := catalog.ArrRequestFailure(err)
	if status == 0 {
		if retryable {
			return &DeliveryError{Target: "Arr rescan", Retryable: true, Reason: "transport unavailable"}
		}
		return &DeliveryError{Target: "Arr rescan", Reason: "request invalid"}
	}
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
		return &DeliveryError{Target: "Arr rescan", StatusCode: status, Retryable: true, Reason: strconv.Itoa(status)}
	}
	// A 4xx is final for this request (a deleted series answers 404, a
	// revoked key 401): retrying would repeat it forever.
	return &DeliveryError{Target: "Arr rescan", StatusCode: status, Refused: true, Reason: strconv.Itoa(status)}
}
