package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"subsyncd/internal/store"
)

// Eligible upgrades can improve provenance without changing subtitle bytes.
// Keep the existing rollback artifact and never roll back a file we did not write.
func (i Installer) refreshIdenticalInstallation(ctx context.Context, request InstallRequest, existing store.Installation) (store.Installation, error) {
	installation := existing
	installation.ProviderID = request.Candidate.ProviderID
	installation.CandidateID = request.Candidate.ResultID
	installation.Fallback = request.Fallback
	fingerprint := request.Media.Fingerprint
	installation.MediaPath = fingerprint.Path
	installation.MediaFileID = fingerprint.FileID
	installation.MediaSize = fingerprint.Size
	installation.MediaModTimeNS = fingerprint.ModTime.UnixNano()
	var err error
	installation.ScoreJSON, err = json.Marshal(request.Score)
	if err != nil {
		return store.Installation{}, fmt.Errorf("encode installation score: %w", err)
	}
	installation.SyncResultJSON, err = json.Marshal(request.SyncResult)
	if err != nil {
		return store.Installation{}, fmt.Errorf("encode synchronization result: %w", err)
	}
	var requests []store.NotificationRequest
	// A replacement media file still needs a new scan even when its sidecar bytes
	// repeat. A same-media provenance refresh does not change Silo's inventory.
	if !InstallationMatchesMedia(existing, request.Media) {
		now := time.Now().UTC()
		if i.Now != nil {
			now = i.Now()
		}
		requests, err = notificationRequests(request.Media, installation, i.NotifierNames, now)
		if err != nil {
			return store.Installation{}, err
		}
	}
	if err := i.inject(StageDatabase); err != nil {
		return store.Installation{}, err
	}
	if err := ctx.Err(); err != nil {
		return store.Installation{}, err
	}
	if err := verifyReplacementUnchanged(installation.Path, existing, true); err != nil {
		return store.Installation{}, err
	}
	if err := verifyMediaUnchanged(fingerprint, i.MediaRoots); err != nil {
		return store.Installation{}, err
	}
	enqueued, err := i.Repository.RecordInstallationWithNotifications(ctx, installation, requests)
	if err != nil {
		return store.Installation{}, fmt.Errorf("record identical subtitle provenance: %w", err)
	}
	i.logEnqueuedNotifications(ctx, enqueued)
	return installation, nil
}
