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
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/asticode/go-astisub"

	"subsyncd/internal/domain"
	"subsyncd/internal/langcheck"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
	"subsyncd/internal/watermark"
)

const maximumInstallBytes int64 = 100 << 20

var ErrProtectedSubtitle = errors.New("subtitle is protected from replacement")

// subtitleValidationError identifies deterministic source-content failures.
// Only validation before staging may return this error; filesystem and commit
// failures must retain their technical error identities.
type subtitleValidationError struct {
	reason string
	// code is a specific rejection reason code; empty means invalid_subtitle.
	code string
}

// minimumRangeCoverage is the share of a multi-episode file's runtime that a
// subtitle must reach to count as covering every episode.
const minimumRangeCoverage = 0.75

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
	// CandidateSignature and ArtifactChecksum are recorded on the installation.
	CandidateSignature string
	ArtifactChecksum   string
}

type Installer struct {
	Repository InstallationStore
	MediaRoots []string
	Mode       os.FileMode
	UID        *int
	GID        *int
	Fault      func(InstallStage) error
	// Link creates the fresh-install hard link; nil uses os.Link.
	Link          func(oldname, newname string) error
	NotifierNames []string
	Now           func() time.Time
	Events        *observability.Emitter
	// StripWatermarks drops subtitle-site advert cues from SRT subtitles.
	StripWatermarks bool
}

func (i Installer) Install(ctx context.Context, request InstallRequest) (store.Installation, error) {
	if i.Repository == nil || request.MediaID <= 0 || request.Language == "" {
		return store.Installation{}, fmt.Errorf("installer repository, media ID, and language are required")
	}
	destination, parent, err := containedDestination(request.DestinationPath, i.MediaRoots)
	if err != nil {
		return store.Installation{}, err
	}
	payload, err := readSubtitleSource(request.SourcePath)
	if err != nil {
		return store.Installation{}, err
	}
	// Advert cues are stripped before validation, so one placed past the end
	// of the media does not fail the overrun check, and the text validated is
	// the text installed. The recorded checksum is of the stripped text, so
	// the installed file still counts as managed and unedited.
	if i.StripWatermarks {
		payload = watermark.Strip(filepath.Ext(request.SourcePath), payload)
	}
	if err := validateSubtitlePayload(request.SourcePath, payload, request.Media); err != nil {
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
	// Linking and renaming keep the file's identity, so this is how the
	// published subtitle is told apart from a later one with the same text.
	stagedInfo, err := stagedFile.Stat()
	if err != nil {
		return failStaging(fmt.Errorf("inspect staged subtitle: %w", err))
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
	published := checksumBytes(payload)
	pendingID, err := i.beginPendingInstall(ctx, store.PendingInstall{MediaID: request.MediaID, Language: request.Language.String(), Destination: destination, Checksum: published, StagedPath: stagedPath, RollbackPath: rollbackPath, PublishedInode: fileInode(stagedInfo)})
	if err != nil {
		if rollbackPath != "" {
			_ = os.Remove(rollbackPath)
		}
		return failStaging(err)
	}
	restore := func(cause error) (store.Installation, error) {
		restoreErrors := []error{cause}
		defer func() {
			// Undone completely: nothing is left for the startup sweep.
			if len(restoreErrors) == 1 {
				i.finishPendingInstall(ctx, pendingID)
			}
		}()
		keepRollback := false
		if committedFile {
			owned, err := i.ownsPublication(ctx, request, destination, stagedInfo, replacing, existing, found)
			switch {
			case err != nil:
				// Unsure whose subtitle the destination holds: leave it and the
				// rollback copy, and keep the pending record for the startup
				// sweep rather than risk overwriting a newer install.
				committedFile = false
				keepRollback = true
				restoreErrors = append(restoreErrors, err)
			case !owned:
				// A run abandoned while publishing can fail its database write
				// long after; another run may have installed a newer subtitle
				// since. Only this run's own publication is rolled back.
				committedFile = false
				i.Events.For("workflow").Log(ctx, slog.LevelWarn, "install.rollback_skipped", "subtitle rollback skipped because the destination changed after publication")
			}
		}
		if committedFile {
			if rollbackPath == "" {
				if removeErr := os.Remove(destination); removeErr != nil {
					restoreErrors = append(restoreErrors, fmt.Errorf("remove newly published subtitle: %w", removeErr))
				} else if syncErr := syncDirectory(parent); syncErr != nil {
					restoreErrors = append(restoreErrors, fmt.Errorf("after removing newly published subtitle: %w", syncErr))
				}
			} else if renameErr := os.Rename(rollbackPath, destination); renameErr != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("restore previous subtitle: %w", renameErr))
			} else {
				rollbackPath = ""
				if syncErr := syncDirectory(parent); syncErr != nil {
					restoreErrors = append(restoreErrors, fmt.Errorf("after restoring previous subtitle: %w", syncErr))
				}
			}
		}
		if rollbackPath != "" && !committedFile && !keepRollback {
			if removeErr := os.Remove(rollbackPath); removeErr != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("remove rollback copy of previous subtitle: %w", removeErr))
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
	// A job abandoned past its time limit keeps running with a canceled
	// context; it must not publish after its search was completed.
	if err := ctx.Err(); err != nil {
		return restore(err)
	}
	if err := i.publishStaged(stagedPath, destination, replacing); err != nil {
		return restore(err)
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
	installation := store.Installation{Fallback: request.Fallback, MediaID: request.MediaID, Language: request.Language.String(), Path: destination, Checksum: published, ProviderID: request.Candidate.ProviderID, CandidateID: request.Candidate.ResultID, CandidateSignature: request.CandidateSignature, ArtifactChecksum: request.ArtifactChecksum, ScoreJSON: scoreJSON, SyncResultJSON: syncJSON, RollbackPath: rollbackPath, MediaPath: fingerprint.Path, MediaFileID: fingerprint.FileID, MediaSize: fingerprint.Size, MediaModTimeNS: fingerprint.ModTime.UnixNano()}
	requests, err := notificationRequests(request.Media, installation, i.NotifierNames, i.now())
	if err != nil {
		return restore(err)
	}
	if err := i.inject(StageDatabase); err != nil {
		return restore(err)
	}
	// Recording clears the pending row in the same transaction.
	record := installation
	record.PendingInstallID = pendingID
	enqueued, err := i.Repository.RecordInstallationWithNotifications(ctx, record, requests)
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

// publishStaged moves the staged subtitle into place. A fresh installation
// links rather than renames, so a sidecar created after the absence check
// (by Arr or the user) is never overwritten. Any other link failure (no
// hard-link support on FUSE, SMB, mergerfs or rclone mounts) falls back to
// rename after the absence check already performed.
func (i Installer) publishStaged(stagedPath, destination string, replacing bool) error {
	if !replacing {
		link := i.Link
		if link == nil {
			link = os.Link
		}
		err := link(stagedPath, destination)
		if err == nil {
			// The subtitle is published. A leftover staging name is removed
			// again by the deferred cleanup and must not undo the install.
			_ = os.Remove(stagedPath)
			return nil
		}
		if errors.Is(err, os.ErrExist) {
			return ErrProtectedSubtitle
		}
	}
	if err := os.Rename(stagedPath, destination); err != nil {
		return fmt.Errorf("publish subtitle: %w", err)
	}
	return nil
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

// readSubtitleSource reads an installation source that is a regular UTF-8
// text file of a sensible size.
func readSubtitleSource(path string) ([]byte, error) {
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
	return payload, nil
}

// validateSubtitlePayload checks the syntax and timing of the text that will
// be installed, against the media's duration.
func validateSubtitlePayload(path string, payload []byte, media domain.Media) error {
	mediaDuration := media.Duration
	subtitles, supported, err := parseSubtitlePayload(path, payload)
	if !supported {
		return &subtitleValidationError{reason: "installation source has unsupported extension"}
	}
	if err != nil || subtitles == nil || len(subtitles.Items) == 0 || len(subtitles.Items) > 100_000 {
		return &subtitleValidationError{reason: "installation source subtitle syntax is invalid"}
	}
	previous := subtitles.Items[0].StartAt
	var last time.Duration
	for _, item := range subtitles.Items {
		if item.StartAt < 0 || item.EndAt < item.StartAt || item.StartAt < previous {
			return &subtitleValidationError{reason: "installation source timestamps are invalid"}
		}
		previous = item.StartAt
		if item.EndAt > last {
			last = item.EndAt
		}
	}
	if mediaDuration > 0 && last > mediaDuration+5*time.Minute {
		return &subtitleValidationError{reason: "installation source extends more than five minutes past media duration"}
	}
	return rangeCoverageError(last, media)
}

// checkRangeCoverage rejects a subtitle for a multi-episode file whose last
// cue ends before minimumRangeCoverage of the runtime: it covers only part of
// the file. Unparseable subtitles are left to the normal validation.
func checkRangeCoverage(path string, media domain.Media) error {
	if !media.IsEpisodeRange() || media.Duration <= 0 {
		return nil
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	subtitles, supported, err := parseSubtitlePayload(path, payload)
	if !supported || err != nil || subtitles == nil {
		return nil
	}
	var last time.Duration
	for _, item := range subtitles.Items {
		last = max(last, item.EndAt)
	}
	return rangeCoverageError(last, media)
}

// parseSubtitlePayload parses a subtitle by its extension. supported is false
// for extensions subsyncd does not install.
func parseSubtitlePayload(path string, payload []byte) (subtitles *astisub.Subtitles, supported bool, err error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".srt":
		subtitles, err = astisub.ReadFromSRT(bytes.NewReader(payload))
	case ".ass", ".ssa":
		subtitles, err = astisub.ReadFromSSAWithOptions(bytes.NewReader(payload), astisub.SSAOptions{})
	case ".vtt":
		subtitles, err = astisub.ReadFromWebVTT(bytes.NewReader(payload))
	default:
		return nil, false, nil
	}
	return subtitles, true, err
}

// checkSubtitleLanguage rejects a subtitle whose text is in a different
// language from the request's (see langcheck). Unreadable files are left to
// the normal validation.
func checkSubtitleLanguage(path string, language domain.Language) error {
	payload, err := os.ReadFile(path)
	if err != nil || int64(len(payload)) > maximumInstallBytes {
		return nil
	}
	if reason, mismatch := langcheck.Check(language.String(), string(payload)); mismatch {
		return &subtitleValidationError{code: "language_mismatch", reason: "subtitle text does not match " + language.String() + ": " + reason}
	}
	return nil
}

func rangeCoverageError(last time.Duration, media domain.Media) error {
	if !media.IsEpisodeRange() || media.Duration <= 0 || float64(last) >= float64(media.Duration)*minimumRangeCoverage {
		return nil
	}
	return &subtitleValidationError{code: "partial_coverage", reason: fmt.Sprintf("subtitle ends at %s of a %s multi-episode file", last.Round(time.Second), media.Duration.Round(time.Second))}
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

// ownsPublication reports whether destination still holds the file this run
// published (published is the staged file's information) and, for a
// replacement, no other run has recorded an installation since, so rolling
// back cannot discard a newer install. The file is compared by identity, not
// content: another run may have published the same text. An error means the
// answer is unknown.
//
// When the destination cannot be inspected, a first install is still treated
// as this run's: no other run publishes over a sidecar it has not recorded, so
// only an outside writer could have replaced it, and leaving it would leave an
// untracked subtitle that blocks every later install. A replacement is not
// restored when unsure, as that could overwrite a newer install.
func (i Installer) ownsPublication(ctx context.Context, request InstallRequest, destination string, published os.FileInfo, replacing bool, existing store.Installation, found bool) (bool, error) {
	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		if !replacing {
			return true, nil
		}
		return false, fmt.Errorf("inspect subtitle destination before rollback: %w", err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(info, published) {
		return false, nil
	}
	if !replacing {
		return true, nil
	}
	// A run paused before its rename can publish after another run installed
	// and recorded a subtitle; its file is then this run's, but the record is
	// the other run's, and the rollback copy is older than both.
	current, currentFound, err := i.Repository.GetInstallation(context.WithoutCancel(ctx), request.MediaID, request.Language)
	if err != nil {
		return false, fmt.Errorf("read installation before rollback: %w", err)
	}
	return currentFound == found && sameInstallationRecord(current, existing), nil
}

func sameInstallationRecord(a, b store.Installation) bool {
	return filepath.Clean(a.Path) == filepath.Clean(b.Path) && a.Checksum == b.Checksum && a.RollbackPath == b.RollbackPath &&
		a.ProviderID == b.ProviderID && a.CandidateID == b.CandidateID
}

// fileInode returns a file's inode number, or 0 when the platform has none.
func fileInode(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Ino)
	}
	return 0
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
