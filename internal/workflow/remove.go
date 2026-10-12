package workflow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

// ErrManagedSubtitleChanged reports a managed subtitle whose file no longer
// matches its record: the owner edited or replaced it, so it is theirs now.
var ErrManagedSubtitleChanged = errors.New("managed subtitle changed on disk")

// removalPrefix names the hidden file a removal moves the subtitle to until
// the record is gone. A pending_installs row whose rollback path carries it is
// a removal, not an install.
const removalPrefix = ".subsyncd-removing-"

// RemovalRequest names a managed subtitle to remove. Media supplies the
// notification payload and the history title; Manual marks the history row.
// Reason, when set, is the history row's reason and the install.removed
// event's: media_replaced or media_renamed for a sidecar left behind by a
// media change (see ReconcileInstallation); a reject leaves it empty.
type RemovalRequest struct {
	Installation store.Installation
	Media        domain.Media
	Manual       bool
	Reason       string
}

// CandidateRemover removes a managed subtitle, as a reject that finds no
// acceptable replacement does.
type CandidateRemover interface {
	RemoveManaged(context.Context, RemovalRequest) error
}

type installationRemover interface {
	RemoveInstallation(context.Context, store.InstallationRemoval) ([]store.NotificationEnqueueResult, error)
}

// RemoveManaged removes a managed subtitle and its record. It acts only on a
// file inside a media root whose content still matches the recorded checksum;
// otherwise it returns ErrManagedSubtitleChanged (or a containment error) and
// changes nothing. The subtitle is first moved aside to a hidden name in its
// directory under a pending_installs row; then one transaction deletes the
// record, writes the history row (outcome removed), and queues the Silo
// notification for the folder. A failed commit moves the subtitle back; a
// crash in between is settled by RecoverPendingInstalls. After the commit the
// hidden file and the installation's rollback copy (an older subtitle that
// was already superseded) are deleted.
func (i Installer) RemoveManaged(ctx context.Context, removal RemovalRequest) error {
	installation := removal.Installation
	remover, ok := i.Repository.(installationRemover)
	if !ok || installation.MediaID <= 0 || installation.Language == "" || installation.Path == "" {
		return fmt.Errorf("installer repository, media ID, language, and subtitle path are required for removal")
	}
	destination, parent, err := containedDestination(installation.Path, i.MediaRoots)
	if err != nil {
		return err
	}
	if err := managedFileMatches(destination, installation.Checksum); err != nil {
		return err
	}
	hidden, err := removalPath(parent)
	if err != nil {
		return err
	}
	rollback := ""
	if installation.RollbackPath != "" {
		if absolute, err := filepath.Abs(installation.RollbackPath); err == nil && filepath.Dir(absolute) == parent {
			rollback = absolute
		}
	}
	// An abandoned job must not change anything.
	if err := ctx.Err(); err != nil {
		return err
	}
	pendingID, err := i.beginPendingInstall(ctx, store.PendingInstall{MediaID: installation.MediaID, Language: installation.Language, Destination: destination, Checksum: installation.Checksum, StagedPath: rollback, RollbackPath: hidden})
	if err != nil {
		return err
	}
	if err := os.Rename(destination, hidden); err != nil {
		i.finishPendingInstall(ctx, pendingID)
		return fmt.Errorf("move managed subtitle aside: %w", observability.PathErrorCause(err))
	}
	restore := func(cause error) error {
		if restoreErr := restoreRemoved(hidden, destination, parent); restoreErr != nil {
			// The pending row stays for the startup sweep.
			return errors.Join(cause, restoreErr)
		}
		i.finishPendingInstall(ctx, pendingID)
		return cause
	}
	// The file could have been edited between the check and the move.
	if err := managedFileMatches(hidden, installation.Checksum); err != nil {
		return restore(err)
	}
	if err := syncDirectory(parent); err != nil {
		return restore(err)
	}
	requests, err := removalNotificationRequests(removal.Media, installation, i.NotifierNames, i.now())
	if err != nil {
		return restore(err)
	}
	if err := i.inject(StageDatabase); err != nil {
		return restore(err)
	}
	detail := store.SearchHistoryDetail{MediaTitle: observability.MediaTitle(removal.Media), ProviderID: installation.ProviderID, Reason: removal.Reason, Manual: removal.Manual}
	enqueued, err := remover.RemoveInstallation(ctx, store.InstallationRemoval{Installation: installation, History: detail, At: i.now(), Notifications: requests})
	if err != nil {
		return restore(fmt.Errorf("record subtitle removal: %w", err))
	}
	i.logEnqueuedNotifications(ctx, enqueued)
	checksum := installation.Checksum
	if len(checksum) > 12 {
		checksum = checksum[:12]
	}
	attrs := []slog.Attr{slog.String("provider", observability.SafeText(installation.ProviderID)), slog.String("candidate_id", observability.SafeText(installation.CandidateID)),
		slog.String("checksum_prefix", observability.SafeText(checksum)), slog.Bool("rollback_deleted", rollback != "")}
	if removal.Reason != "" {
		attrs = append(attrs, slog.String("reason", removal.Reason))
	}
	i.Events.For("workflow").Log(ctx, slog.LevelInfo, "install.removed", "managed subtitle removed", attrs...)
	if err := i.inject(StageCleanup); err != nil {
		// The record is gone; the startup sweep deletes what is left.
		return nil
	}
	cleanup := []error{removeContainedFile(hidden, parent)}
	if rollback != "" {
		cleanup = append(cleanup, removeContainedFile(rollback, parent))
	}
	if errors.Join(cleanup...) == nil {
		_ = syncDirectory(parent)
		i.finishPendingInstall(ctx, pendingID)
	}
	return nil
}

// managedFileMatches reports whether path is a regular file with the
// recorded checksum.
func managedFileMatches(path, checksum string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrManagedSubtitleChanged
		}
		return fmt.Errorf("inspect managed subtitle: %w", observability.PathErrorCause(err))
	}
	if !info.Mode().IsRegular() {
		return ErrManagedSubtitleChanged
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read managed subtitle: %w", observability.PathErrorCause(err))
	}
	if checksum == "" || checksumBytes(payload) != checksum {
		return ErrManagedSubtitleChanged
	}
	return nil
}

func removalPath(parent string) (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("name removed subtitle: %w", err)
	}
	return filepath.Join(parent, removalPrefix+hex.EncodeToString(value[:])), nil
}

// restoreRemoved moves a subtitle set aside by a removal back to its
// destination. It links rather than renames, so a file created at the
// destination meanwhile is never overwritten; without hard links it renames
// after checking the destination is still free.
func restoreRemoved(hidden, destination, parent string) error {
	if err := os.Link(hidden, destination); err == nil {
		if err := os.Remove(hidden); err != nil {
			return fmt.Errorf("remove set-aside subtitle after restoring it: %w", observability.PathErrorCause(err))
		}
	} else if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("restore removed subtitle: destination was recreated")
	} else if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("restore removed subtitle: destination was recreated")
	} else if err := os.Rename(hidden, destination); err != nil {
		return fmt.Errorf("restore removed subtitle: %w", observability.PathErrorCause(err))
	}
	return syncDirectory(parent)
}

// isPendingRemoval reports whether a pending_installs row records a removal.
func isPendingRemoval(item store.PendingInstall) bool {
	return strings.HasPrefix(filepath.Base(item.RollbackPath), removalPrefix)
}

// recoverPendingRemoval settles a removal a crash interrupted. While the
// record is still there the removal did not commit, and the subtitle is put
// back (unless the destination was recreated, which wins). Once the record is
// gone the set-aside subtitle and the old rollback copy are deleted.
func (i Installer) recoverPendingRemoval(ctx context.Context, item store.PendingInstall, destination, parent string) (string, error) {
	installation, found, err := i.Repository.GetInstallation(ctx, item.MediaID, domain.Language(item.Language))
	if err != nil {
		return "", fmt.Errorf("read installation: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	committed := !found || filepath.Clean(installation.Path) != destination || installation.Checksum != item.Checksum
	if !committed && regularFile(item.RollbackPath) {
		if _, statErr := os.Lstat(destination); errors.Is(statErr, os.ErrNotExist) {
			if err := restoreRemoved(item.RollbackPath, destination, parent); err != nil {
				return "", err
			}
			return "removal_undone", nil
		}
	}
	cleanup := []error{removeContainedFile(item.RollbackPath, parent)}
	if committed && item.StagedPath != "" {
		cleanup = append(cleanup, removeContainedFile(item.StagedPath, parent))
	}
	if err := errors.Join(cleanup...); err != nil {
		return "", err
	}
	if committed {
		return "removed", nil
	}
	return "removal_cleaned", nil
}

// removalNotificationRequests asks each notifier to rescan the folder after a
// removal. The dedupe key differs from the install's, which may still be in
// the outbox, and from any earlier removal of the same file.
func removalNotificationRequests(media domain.Media, installation store.Installation, notifierNames []string, now time.Time) ([]store.NotificationRequest, error) {
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
		raw := fmt.Sprintf("removed\x00%s\x00%d\x00%s\x00%s\x00%s\x00%d", name, installation.MediaID, installation.Language, installation.Checksum, installation.Path, now.UnixNano())
		sum := sha256.Sum256([]byte(raw))
		requests = append(requests, store.NotificationRequest{Notifier: name, DedupeKey: hex.EncodeToString(sum[:]), PayloadJSON: payload, NextAttemptAt: now})
	}
	return requests, nil
}
