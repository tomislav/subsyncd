package app

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

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

// maxMovePlanBytes bounds the move plan file.
const maxMovePlanBytes = 4 << 20

// MovePlan is the file move-media reads: the entries to move from one
// instance to another. Unknown fields are refused.
type MovePlan struct {
	Entries []MovePlanEntry `json:"entries"`
}

// MovePlanEntry names a source file by identity (tmdb_id for a movie;
// tvdb_id, season and episode for an episode) or by from_file_id, and gives
// its identity and path under the target instance. to_path is the path as
// the target Arr reports it, or as this container sees it.
type MovePlanEntry struct {
	Kind       string `json:"kind"`
	TMDBID     int64  `json:"tmdb_id,omitempty"`
	TVDBID     int64  `json:"tvdb_id,omitempty"`
	Season     int    `json:"season,omitempty"`
	Episode    int    `json:"episode,omitempty"`
	FromFileID int64  `json:"from_file_id,omitempty"`
	ToEntityID int64  `json:"to_entity_id"`
	ToFileID   int64  `json:"to_file_id"`
	ToSeriesID int64  `json:"to_series_id,omitempty"`
	ToPath     string `json:"to_path"`
}

// MoveMedia moves media rows of the instance from to the instance to in
// place, as the plan at planPath lists them, after the media files and
// their sidecars were moved on disk (renamed into the target library, same
// file names). Each row keeps its id, so its installations, searches and
// history stay attached, and the next reconciliation of to finds every
// installation unchanged. Without apply it only checks and reports, and
// works on a read-only application; with apply it takes the process lock, so
// the daemon must be stopped. Any problem in any entry refuses the whole move.
func (a *App) MoveMedia(ctx context.Context, from, to, planPath string, apply bool) (control.MoveMediaResult, error) {
	usage := func(format string, args ...any) error {
		return &control.Error{Code: control.CodeUsage, Message: fmt.Sprintf(format, args...)}
	}
	if from == to {
		return control.MoveMediaResult{}, usage("--from and --to must name different instances")
	}
	var target *config.InstanceConfig
	for index := range a.Config.Instances {
		if a.Config.Instances[index].Name == to {
			target = &a.Config.Instances[index]
		}
	}
	if target == nil {
		return control.MoveMediaResult{}, &control.Error{Code: control.CodeUnknownInstance, Message: fmt.Sprintf("unknown instance %q: --to must be configured", to)}
	}
	sourceType := ""
	for _, instance := range a.Config.Instances {
		if instance.Name == from {
			sourceType = instance.Type
		}
	}
	storedType, known, err := a.Repository.InstanceType(ctx, from)
	if err != nil {
		return control.MoveMediaResult{}, err
	}
	if !known {
		return control.MoveMediaResult{}, &control.Error{Code: control.CodeUnknownInstance, Message: fmt.Sprintf("unknown instance %q: the database has no records for it", from)}
	}
	if sourceType == "" {
		sourceType = storedType
	}
	if sourceType != "" && sourceType != target.Type {
		return control.MoveMediaResult{}, usage("--from is a %s instance and --to a %s instance", sourceType, target.Type)
	}
	kind := domain.MediaMovie
	if target.Type == "sonarr" {
		kind = domain.MediaEpisode
	}

	plan, err := readMovePlan(planPath)
	if err != nil {
		return control.MoveMediaResult{}, usage("%v", err)
	}
	entries := make([]store.MediaMoveEntry, len(plan.Entries))
	for index, item := range plan.Entries {
		if item.Kind != string(kind) {
			return control.MoveMediaResult{}, usage("plan entry %d: kind %q does not match the %s instance %s", index+1, item.Kind, target.Type, to)
		}
		entries[index] = store.MediaMoveEntry{Kind: kind, TMDBID: item.TMDBID, TVDBID: item.TVDBID, Season: item.Season, Episode: item.Episode, FromFileID: item.FromFileID, ToEntityID: item.ToEntityID, ToFileID: item.ToFileID, ToSeriesID: item.ToSeriesID, ToPath: a.localMovePath(item.ToPath, target.PathMappings)}
	}

	if apply {
		release, err := a.acquireMutationLock()
		if err != nil {
			return control.MoveMediaResult{}, err
		}
		defer release()
	}
	items, err := a.Repository.MoveMedia(ctx, from, to, entries, a.checkMovedFiles, apply)
	if errors.Is(err, store.ErrMediaMoveWebhooksWaiting) {
		return control.MoveMediaResult{}, &control.Error{Code: control.CodeMoveRefused, Message: fmt.Sprintf("move refused: webhooks from %s or %s are stored but not applied yet; nothing was moved. Start subsyncd with the files still in place to apply them, then stop it and move the files", from, to)}
	}
	refused := errors.Is(err, store.ErrMediaMoveRefused)
	if err != nil && !refused {
		return control.MoveMediaResult{}, err
	}
	result := control.MoveMediaResult{From: from, To: to, Applied: apply && !refused, Ready: !refused, Entries: make([]control.MoveMediaEntry, 0, len(items))}
	for _, item := range items {
		result.Entries = append(result.Entries, control.MoveMediaEntry{
			Entry: item.Entry, Kind: string(item.Plan.Kind), Title: item.Title, Year: item.Year, Season: item.Season, Episode: item.Episode,
			MediaID: item.MediaID, FromFileID: item.FromFileID, ToFileID: item.Plan.ToFileID, ToEntityID: item.Plan.ToEntityID,
			Installations: len(item.Sidecars), Tracks: item.Tracks, PendingNotifications: item.PendingNotifications, Problems: append([]string{}, item.Problems...),
		})
		if item.MediaID != 0 {
			result.Media++
		}
		result.Installations += len(item.Sidecars)
		result.Tracks += item.Tracks
		result.Problems += len(item.Problems)
	}
	if refused && apply {
		return result, &control.Error{Code: control.CodeMoveRefused, Message: fmt.Sprintf("move refused: %d problems in %d entries; nothing was moved. Run without --yes for the report. %s", result.Problems, len(result.Entries), firstMoveProblem(result))}
	}
	if !result.Applied {
		return result, nil
	}
	a.Events.For("app").Log(ctx, slog.LevelInfo, "media.moved", "moved media records to another instance",
		slog.String("from", observability.SafeText(from)),
		slog.String("to", observability.SafeText(to)),
		slog.Int("media_count", result.Media),
		slog.Int("installation_count", result.Installations),
		slog.Int("track_count", result.Tracks),
	)
	return result, nil
}

func firstMoveProblem(result control.MoveMediaResult) string {
	for _, entry := range result.Entries {
		if len(entry.Problems) > 0 {
			return fmt.Sprintf("First: entry %d: %s.", entry.Entry, entry.Problems[0])
		}
	}
	return ""
}

func readMovePlan(path string) (MovePlan, error) {
	if path == "" {
		return MovePlan{}, errors.New("--map is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return MovePlan{}, fmt.Errorf("open move plan: %v", observability.PathErrorCause(err))
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, maxMovePlanBytes+1))
	if err != nil {
		return MovePlan{}, fmt.Errorf("read move plan: %v", observability.PathErrorCause(err))
	}
	if len(payload) > maxMovePlanBytes {
		return MovePlan{}, fmt.Errorf("move plan is larger than %d bytes", maxMovePlanBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var plan MovePlan
	if err := decoder.Decode(&plan); err != nil {
		return MovePlan{}, fmt.Errorf("parse move plan: %v", err)
	}
	if decoder.More() {
		return MovePlan{}, errors.New("parse move plan: unexpected data after the plan")
	}
	if len(plan.Entries) == 0 {
		return MovePlan{}, errors.New("move plan has no entries")
	}
	return plan, nil
}

// localMovePath maps a to_path the target Arr reports through its path
// mappings; a path no mapping matches is taken as a path in this container.
func (a *App) localMovePath(path string, mappings []config.PathMapping) string {
	if mapped, err := catalog.MapPath(path, mappings, a.Config.MediaRoots); err == nil {
		return mapped
	}
	return path
}

// checkMovedFiles checks the files of a move on disk: the media file is at
// its new path (a regular file in a media root, the recorded size) and gone
// from the old one, and every sidecar is at its new path with the recorded
// checksum, as is any rollback copy.
func (a *App) checkMovedFiles(_ context.Context, item *store.MediaMoveItem) {
	problem := func(format string, args ...any) {
		item.Problems = append(item.Problems, fmt.Sprintf(format, args...))
	}
	if !a.inMediaRoot(item.ToPath) {
		problem("to_path is outside the configured media roots")
		return
	}
	info, err := os.Lstat(item.ToPath)
	switch {
	case err != nil:
		problem("the media file is not at to_path (%v)", observability.PathErrorCause(err))
	case !info.Mode().IsRegular():
		problem("to_path is not a regular file")
	case info.Size() != item.Size:
		problem("the file at to_path has %d bytes, the recorded file %d", info.Size(), item.Size)
	}
	if _, err := os.Lstat(item.FromPath); err == nil {
		problem("the media file is still at its old path; move it first")
	} else if !errors.Is(err, os.ErrNotExist) {
		problem("the old media path cannot be checked (%v)", observability.PathErrorCause(err))
	}
	for _, sidecar := range item.Sidecars {
		if err := fileHasChecksum(sidecar.To, sidecar.Checksum); err != nil {
			problem("the %s sidecar %s: %v", sidecar.Language, filepath.Base(sidecar.To), err)
		}
		if sidecar.RollbackTo == "" {
			continue
		}
		if info, err := os.Lstat(sidecar.RollbackTo); err != nil || !info.Mode().IsRegular() {
			problem("the %s rollback copy %s is not at the new path", sidecar.Language, filepath.Base(sidecar.RollbackTo))
		}
	}
}

// inMediaRoot reports whether path's folder, symlinks resolved, is inside a
// configured media root.
func (a *App) inMediaRoot(path string) bool {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return false
	}
	for _, root := range a.Config.MediaRoots {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(resolved, parent)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// fileHasChecksum reports why path is not a regular file whose SHA-256 is
// checksum, or nil.
func fileHasChecksum(path, checksum string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("is not at the new path")
	}
	if !info.Mode().IsRegular() {
		return errors.New("is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot be read (%v)", observability.PathErrorCause(err))
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return fmt.Errorf("cannot be read (%v)", observability.PathErrorCause(err))
	}
	if hex.EncodeToString(digest.Sum(nil)) != checksum {
		return errors.New("has changed since subsyncd installed it")
	}
	return nil
}
