package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/notifier"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

// NotificationPayload is the durable, credential-free delivery contract shared
// by all installation entry points and the notification worker.
type NotificationPayload struct {
	Media        domain.Media `json:"media"`
	SubtitlePath string       `json:"subtitle_path"`
}

func notificationRequests(media domain.Media, installation store.Installation, notifierNames []string, now time.Time) ([]store.NotificationRequest, error) {
	if len(notifierNames) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(NotificationPayload{Media: media, SubtitlePath: installation.Path})
	if err != nil {
		return nil, fmt.Errorf("encode notification payload: %w", err)
	}
	names := append([]string(nil), notifierNames...)
	sort.Strings(names)
	requests := make([]store.NotificationRequest, 0, len(names))
	for _, name := range names {
		// Identical subtitle bytes still need a scan after media replacement or
		// publication at a different destination. Use the committed fingerprint;
		// delivery time and mutable scoring metadata must not affect replay.
		raw := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d",
			name, installation.MediaID, installation.Language, installation.Checksum,
			installation.Path, installation.MediaPath, installation.MediaFileID,
			installation.MediaSize, installation.MediaModTimeNS)
		sum := sha256.Sum256([]byte(raw))
		requests = append(requests, store.NotificationRequest{Notifier: name, DedupeKey: hex.EncodeToString(sum[:]), PayloadJSON: payload, NextAttemptAt: notificationDue(name, now)})
	}
	return requests, nil
}

// notificationDue is when a notification queued at now is first due: an Arr
// rescan after notifier.ArrRescanDelay, so close changes coalesce; any other
// at once.
func notificationDue(name string, now time.Time) time.Time {
	if name == notifier.ArrRescanName {
		return now.Add(notifier.ArrRescanDelay)
	}
	return now
}

// changeNotifierNames returns the notifiers to queue after a change on disk to
// a sidecar of media: the configured ones, plus a rescan by the owning Arr
// instance when its rescan_after_changes is on and the series or movie ID is
// known. Without the ID the rescan is skipped (logged), never the change.
func (i Installer) changeNotifierNames(ctx context.Context, media domain.Media) []string {
	names := append([]string(nil), i.NotifierNames...)
	if !i.ArrRescanInstances[media.Ref.Instance] {
		return names
	}
	if _, ok := notifier.RescanID(media); !ok {
		i.Events.For("workflow").Log(ctx, slog.LevelInfo, "notification.skipped", "Arr rescan skipped: the series or movie ID is unknown",
			slog.String("notifier", notifier.ArrRescanName), slog.String("instance", observability.SafeText(media.Ref.Instance)),
			slog.String("media_kind", string(media.Ref.Kind)), slog.String("outcome", "skipped"), slog.String("reason", notifier.SkipUnknownArrID))
		return names
	}
	return append(names, notifier.ArrRescanName)
}

// arrRescanOnly returns the Arr rescan among a change's notifier names, for a
// change only Arr needs to hear about (a sidecar moved after a rename).
func arrRescanOnly(names []string) []string {
	for _, name := range names {
		if name == notifier.ArrRescanName {
			return []string{name}
		}
	}
	return nil
}

// changeNotificationRequests queues one intent per notifier for a sidecar
// change other than an install (a removal, a move). The dedupe key differs
// from the install's, which may still be in the outbox, and from any earlier
// change of the same file.
func changeNotificationRequests(change string, media domain.Media, installation store.Installation, notifierNames []string, now time.Time) ([]store.NotificationRequest, error) {
	if len(notifierNames) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(NotificationPayload{Media: media, SubtitlePath: installation.Path})
	if err != nil {
		return nil, fmt.Errorf("encode notification payload: %w", err)
	}
	names := append([]string(nil), notifierNames...)
	sort.Strings(names)
	requests := make([]store.NotificationRequest, 0, len(names))
	for _, name := range names {
		raw := fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%d", change, name, installation.MediaID, installation.Language, installation.Checksum, installation.Path, now.UnixNano())
		sum := sha256.Sum256([]byte(raw))
		requests = append(requests, store.NotificationRequest{Notifier: name, DedupeKey: hex.EncodeToString(sum[:]), PayloadJSON: payload, NextAttemptAt: notificationDue(name, now)})
	}
	return requests, nil
}
