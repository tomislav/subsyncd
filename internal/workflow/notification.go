package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"subsyncd/internal/domain"
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
		requests = append(requests, store.NotificationRequest{Notifier: name, DedupeKey: hex.EncodeToString(sum[:]), PayloadJSON: payload, NextAttemptAt: now})
	}
	return requests, nil
}
