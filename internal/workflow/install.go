package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/asticode/go-astisub"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

const maximumInstallBytes int64 = 100 << 20

var ErrProtectedSubtitle = errors.New("subtitle is protected from replacement")

// subtitleValidationError identifies deterministic source-content failures.
// Only validation before staging may return this error; filesystem and commit
// failures must retain their technical error identities.
type subtitleValidationError struct{ reason string }

func (e *subtitleValidationError) Error() string { return e.reason }

type InstallStage string

const (
	StageCreate        InstallStage = "create"
	StageWrite         InstallStage = "write"
	StageFileSync      InstallStage = "file_sync"
	StageChmod         InstallStage = "chmod"
	StageRename        InstallStage = "rename"
	StageDirectorySync InstallStage = "directory_sync"
	StageDatabase      InstallStage = "database"
	StageCleanup       InstallStage = "cleanup"
)

type InstallationStore interface {
	GetInstallation(context.Context, int64, domain.Language) (store.Installation, bool, error)
	RecordInstallationWithNotifications(context.Context, store.Installation, []store.NotificationRequest) ([]store.NotificationEnqueueResult, error)
}

type InstallRequest struct {
	Fallback        bool
	MediaID         int64
	Media           domain.Media
	Language        domain.Language
	SourcePath      string
	DestinationPath string
	Candidate       domain.Candidate
	Score           domain.Score
	SyncResult      domain.SyncResult
}

type Installer struct {
	Repository    InstallationStore
	MediaRoots    []string
	Mode          os.FileMode
	UID           *int
	GID           *int
	Fault         func(InstallStage) error
	NotifierNames []string
	Now           func() time.Time
	Events        *observability.Emitter
}

func (i Installer) Install(ctx context.Context, request InstallRequest) (store.Installation, error) {
	if i.Repository == nil || request.MediaID <= 0 || request.Language == "" {
		return store.Installation{}, fmt.Errorf("installer repository, media ID, and language are required")
	}
	destination, parent, err := containedDestination(request.DestinationPath, i.MediaRoots)
	if err != nil {
		return store.Installation{}, err
	}
	payload, err := validatedSubtitle(request.SourcePath, request.Media.Duration)
	if err != nil {
		return store.Installation{}, err
	}
	existing, found, err := i.Repository.GetInstallation(ctx, request.MediaID, request.Language)
	if err != nil {
		return store.Installation{}, fmt.Errorf("read existing installation: %w", err)
	}
	replacing, err := replacementState(destination, existing, found)
	if err != nil {
		return store.Installation{}, err
	}

	if replacing && checksumBytes(payload) == existing.Checksum {
		return i.refreshIdenticalInstallation(ctx, request, existing)
	}

	if err := i.inject(StageCreate); err != nil {
		return store.Installation{}, err
	}
	stagedFile, err := os.CreateTemp(parent, ".subsyncd-stage-")
	if err != nil {
		return store.Installation{}, fmt.Errorf("create staged subtitle: %w", err)
	}
	stagedPath := stagedFile.Name()
	defer func() { _ = os.Remove(stagedPath) }()
	closeStaged := func() error {
		if stagedFile == nil {
			return nil
		}
		err := stagedFile.Close()
		stagedFile = nil
		return err
	}
	failStaging := func(cause error) (store.Installation, error) {
		_ = closeStaged()
		_ = os.Remove(stagedPath)
		return store.Installation{}, cause
	}
	if err := i.inject(StageWrite); err != nil {
		return failStaging(err)
	}
	if err := writeAll(stagedFile, payload); err != nil {
		return failStaging(fmt.Errorf("write staged subtitle: %w", err))
	}
	if err := i.inject(StageFileSync); err != nil {
		return failStaging(err)
	}
	if err := stagedFile.Sync(); err != nil {
		return failStaging(fmt.Errorf("sync staged subtitle: %w", err))
	}
	if err := i.inject(StageChmod); err != nil {
		return failStaging(err)
	}
	mode := i.Mode.Perm()
	if mode == 0 {
		mode = 0o644
	}
	if err := stagedFile.Chmod(mode); err != nil {
		return failStaging(fmt.Errorf("chmod staged subtitle: %w", err))
	}
	if i.UID != nil || i.GID != nil {
		uid, gid := -1, -1
		if i.UID != nil {
			uid = *i.UID
		}
		if i.GID != nil {
			gid = *i.GID
		}
		if err := stagedFile.Chown(uid, gid); err != nil {
			return failStaging(fmt.Errorf("chown staged subtitle: %w", err))
		}
	}
	if err := closeStaged(); err != nil {
		return failStaging(fmt.Errorf("close staged subtitle: %w", err))
	}

	rollbackPath := ""
	if replacing {
		rollbackPath, err = copyRollback(destination, parent)
		if err != nil {
			return failStaging(err)
		}
	}
	committedFile := false
	restore := func(cause error) (store.Installation, error) {
		restoreErrors := []error{cause}
		if committedFile {
			if rollbackPath == "" {
				if removeErr := os.Remove(destination); removeErr != nil {
					restoreErrors = append(restoreErrors, fmt.Errorf("remove newly published subtitle: %w", removeErr))
				} else if syncErr := syncDirectory(parent); syncErr != nil {
					restoreErrors = append(restoreErrors, fmt.Errorf("sync subtitle directory after rollback: %w", syncErr))
				}
			} else if renameErr := os.Rename(rollbackPath, destination); renameErr != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("restore previous subtitle: %w", renameErr))
			} else {
				rollbackPath = ""
				if syncErr := syncDirectory(parent); syncErr != nil {
					restoreErrors = append(restoreErrors, fmt.Errorf("sync subtitle directory after rollback: %w", syncErr))
				}
			}
		}
		if rollbackPath != "" && !committedFile {
			if removeErr := os.Remove(rollbackPath); removeErr != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("remove unused rollback subtitle: %w", removeErr))
			}
		}
		_ = os.Remove(stagedPath)
		return store.Installation{}, errors.Join(restoreErrors...)
	}
	if err := verifyReplacementUnchanged(destination, existing, replacing); err != nil {
		return restore(err)
	}
	if err := i.inject(StageRename); err != nil {
		return restore(err)
	}
	if err := verifyMediaUnchanged(request.Media.Fingerprint, i.MediaRoots); err != nil {
		return restore(err)
	}
	if err := os.Rename(stagedPath, destination); err != nil {
		return restore(fmt.Errorf("publish subtitle: %w", err))
	}
	committedFile = true
	if err := i.inject(StageDirectorySync); err != nil {
		return restore(err)
	}
	if err := syncDirectory(parent); err != nil {
		return restore(err)
	}
	scoreJSON, err := json.Marshal(request.Score)
	if err != nil {
		return restore(fmt.Errorf("encode installation score: %w", err))
	}
	syncJSON, err := json.Marshal(request.SyncResult)
	if err != nil {
		return restore(fmt.Errorf("encode synchronization result: %w", err))
	}
	fingerprint := request.Media.Fingerprint
	installation := store.Installation{Fallback: request.Fallback, MediaID: request.MediaID, Language: request.Language.String(), Path: destination, Checksum: checksumBytes(payload), ProviderID: request.Candidate.ProviderID, CandidateID: request.Candidate.ResultID, ScoreJSON: scoreJSON, SyncResultJSON: syncJSON, RollbackPath: rollbackPath, MediaPath: fingerprint.Path, MediaFileID: fingerprint.FileID, MediaSize: fingerprint.Size, MediaModTimeNS: fingerprint.ModTime.UnixNano()}
	now := time.Now().UTC()
	if i.Now != nil {
		now = i.Now()
	}
	requests, err := notificationRequests(request.Media, installation, i.NotifierNames, now)
	if err != nil {
		return restore(err)
	}
	if err := i.inject(StageDatabase); err != nil {
		return restore(err)
	}
	enqueued, err := i.Repository.RecordInstallationWithNotifications(ctx, installation, requests)
	if err != nil {
		return restore(fmt.Errorf("record installed subtitle: %w", err))
	}
	i.logEnqueuedNotifications(ctx, enqueued)
	if found && existing.RollbackPath != "" && filepath.Clean(existing.RollbackPath) != filepath.Clean(rollbackPath) {
		if err := i.inject(StageCleanup); err == nil {
			_ = removeContainedFile(existing.RollbackPath, parent)
		}
	}
	return installation, nil
}

func (i Installer) inject(stage InstallStage) error {
	if i.Fault == nil {
		return nil
	}
	if err := i.Fault(stage); err != nil {
		return fmt.Errorf("%s installation stage: %w", stage, err)
	}
	return nil
}

func verifyMediaUnchanged(fingerprint domain.MediaFingerprint, roots []string) error {
	resolved, err := filepath.EvalSymlinks(fingerprint.Path)
	if err != nil {
		return fmt.Errorf("media changed before subtitle publication")
	}
	if _, _, err := containedDestination(resolved, roots); err != nil {
		return fmt.Errorf("media is outside configured roots before subtitle publication")
	}
	info, err := os.Stat(fingerprint.Path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != fingerprint.Size || !info.ModTime().Equal(fingerprint.ModTime) {
		return fmt.Errorf("media changed before subtitle publication")
	}
	return nil
}

func replacementState(destination string, existing store.Installation, found bool) (bool, error) {
	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect subtitle destination: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, ErrProtectedSubtitle
	}
	if !found || filepath.Clean(existing.Path) != filepath.Clean(destination) {
		return false, ErrProtectedSubtitle
	}
	payload, err := os.ReadFile(destination)
	if err != nil {
		return false, fmt.Errorf("read managed subtitle: %w", err)
	}
	if checksumBytes(payload) != existing.Checksum {
		return false, ErrProtectedSubtitle
	}
	return true, nil
}

func verifyReplacementUnchanged(destination string, existing store.Installation, replacing bool) error {
	if !replacing {
		if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
			return ErrProtectedSubtitle
		}
		return nil
	}
	info, err := os.Lstat(destination)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrProtectedSubtitle
	}
	payload, err := os.ReadFile(destination)
	if err != nil || checksumBytes(payload) != existing.Checksum {
		return ErrProtectedSubtitle
	}
	return nil
}

func containedDestination(path string, roots []string) (string, string, error) {
	if len(roots) == 0 {
		return "", "", fmt.Errorf("at least one media root is required")
	}
	destination, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve subtitle destination: %w", err)
	}
	parent := filepath.Dir(destination)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", "", fmt.Errorf("resolve subtitle destination parent: %w", err)
	}
	contained := false
	for _, root := range roots {
		absoluteRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		resolvedRoot, err := filepath.EvalSymlinks(absoluteRoot)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(resolvedRoot, resolvedParent)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			contained = true
			break
		}
	}
	if !contained {
		return "", "", fmt.Errorf("subtitle destination is outside configured media roots")
	}
	return destination, parent, nil
}

func validatedSubtitle(path string, mediaDuration time.Duration) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("installation source is not a regular file")
	}
	if info.Size() <= 0 || info.Size() > maximumInstallBytes {
		return nil, &subtitleValidationError{reason: "installation source size is invalid"}
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read installation source: %w", err)
	}
	if !utf8.Valid(payload) || bytes.IndexByte(payload, 0) >= 0 {
		return nil, &subtitleValidationError{reason: "installation source is not UTF-8 text"}
	}
	var subtitles *astisub.Subtitles
	switch strings.ToLower(filepath.Ext(path)) {
	case ".srt":
		subtitles, err = astisub.ReadFromSRT(bytes.NewReader(payload))
	case ".ass", ".ssa":
		subtitles, err = astisub.ReadFromSSAWithOptions(bytes.NewReader(payload), astisub.SSAOptions{})
	case ".vtt":
		subtitles, err = astisub.ReadFromWebVTT(bytes.NewReader(payload))
	default:
		return nil, &subtitleValidationError{reason: "installation source has unsupported extension"}
	}
	if err != nil || subtitles == nil || len(subtitles.Items) == 0 || len(subtitles.Items) > 100_000 {
		return nil, &subtitleValidationError{reason: "installation source subtitle syntax is invalid"}
	}
	previous := subtitles.Items[0].StartAt
	var last time.Duration
	for _, item := range subtitles.Items {
		if item.StartAt < 0 || item.EndAt < item.StartAt || item.StartAt < previous {
			return nil, &subtitleValidationError{reason: "installation source timestamps are invalid"}
		}
		previous = item.StartAt
		if item.EndAt > last {
			last = item.EndAt
		}
	}
	if mediaDuration > 0 && last > mediaDuration+5*time.Minute {
		return nil, &subtitleValidationError{reason: "installation source extends more than five minutes past media duration"}
	}
	return payload, nil
}

func copyRollback(source, parent string) (string, error) {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("subtitle rollback source is not a regular file")
	}
	payload, err := os.ReadFile(source)
	if err != nil {
		return "", fmt.Errorf("read subtitle rollback: %w", err)
	}
	file, err := os.CreateTemp(parent, ".subsyncd-rollback-")
	if err != nil {
		return "", fmt.Errorf("create subtitle rollback: %w", err)
	}
	path := file.Name()
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	if err := writeAll(file, payload); err != nil {
		return "", fmt.Errorf("write subtitle rollback: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync subtitle rollback: %w", err)
	}
	if err := file.Chmod(info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("chmod subtitle rollback: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close subtitle rollback: %w", err)
	}
	cleanup = false
	return path, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open subtitle directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync subtitle directory: %w", err)
	}
	return nil
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) != 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		payload = payload[written:]
	}
	return nil
}

func checksumBytes(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func removeContainedFile(path, parent string) error {
	absolute, err := filepath.Abs(path)
	if err != nil || filepath.Dir(absolute) != filepath.Clean(parent) {
		return fmt.Errorf("cleanup path is outside subtitle directory")
	}
	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("cleanup path is not a regular file")
	}
	return os.Remove(absolute)
}

func (i Installer) logEnqueuedNotifications(ctx context.Context, enqueued []store.NotificationEnqueueResult) {
	for _, result := range enqueued {
		if result.Inserted {
			key := result.DedupeKey
			if len(key) > 12 {
				key = key[:12]
			}
			i.Events.For("workflow").Log(ctx, slog.LevelInfo, "notification.queued", "subtitle notification queued",
				slog.String("notifier", result.Notifier), slog.String("notification_key", key))
		}
	}
}
