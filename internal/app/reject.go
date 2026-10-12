package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
	"subsyncd/internal/workflow"
)

// rejectTarget is the managed subtitle a reject acts on.
type rejectTarget struct {
	mediaID      int64
	media        domain.Media
	language     domain.Language
	installation store.Installation
	reason       string
	undo         bool
	// jobID owns the follow-up search's lease from the durable step on.
	jobID string
}

// Reject records the owner's rejection of a managed subtitle and runs the
// follow-up search in this process, waiting for it like search. A running
// daemon executes the command itself (rejectInDaemon).
func (a *App) Reject(ctx context.Context, args control.RejectArgs) (control.RejectResult, error) {
	if args.Wait != 0 || args.NoWait {
		return control.RejectResult{}, &control.Error{Code: control.CodeDaemonRequired, Message: "--timeout and --no-wait need a running daemon; without one, reject runs to completion"}
	}
	release, err := a.acquireMutationLock()
	if err != nil {
		return control.RejectResult{}, err
	}
	defer release()
	target, err := a.prepareReject(ctx, args)
	if err != nil {
		return control.RejectResult{}, err
	}
	if target.undo {
		return a.undoReject(ctx, target)
	}
	if err := a.recordReject(ctx, target); err != nil {
		return control.RejectResult{}, err
	}
	outcome, err := a.runManualSearch(ctx, target.mediaID, target.media, target.language, manualOptions{jobID: target.jobID})
	if err != nil {
		return control.RejectResult{}, err
	}
	return control.RejectResult{MediaID: target.mediaID, Rejected: a.languageOutcome(target.language, outcome)}, nil
}

// rejectInDaemon records the rejection on the request, then runs the
// follow-up search on the daemon's run context and waits for it like
// searchInDaemon: up to args.Wait (5 minutes when unset, none with NoWait),
// reporting running when the wait ends first.
func (a *App) rejectInDaemon(ctx context.Context, args control.RejectArgs) (control.RejectResult, []slog.Attr, error) {
	wait := args.Wait
	switch {
	case args.NoWait && wait != 0:
		return control.RejectResult{}, nil, &control.Error{Code: control.CodeUsage, Message: "use only one of --timeout and --no-wait"}
	case wait < 0:
		return control.RejectResult{}, nil, &control.Error{Code: control.CodeUsage, Message: "--timeout must be positive"}
	case wait == 0 && !args.NoWait:
		wait = defaultSearchWait
	}
	runCtx := a.serveCtx
	if runCtx == nil || runCtx.Err() != nil {
		return control.RejectResult{}, nil, &control.Error{Code: control.CodeDaemonStopping, Message: "subsyncd is shutting down"}
	}
	target, err := a.prepareReject(ctx, args)
	if err != nil {
		return control.RejectResult{}, nil, err
	}
	attrs := []slog.Attr{slog.String("media_title", observability.MediaTitle(target.media)), slog.String("language", target.language.String())}
	if target.undo {
		result, err := a.undoReject(ctx, target)
		return result, attrs, err
	}
	if err := a.recordReject(ctx, target); err != nil {
		return control.RejectResult{}, nil, err
	}
	type finished struct {
		result workflow.Result
		err    error
	}
	done := make(chan finished, 1)
	a.manualRuns.Add(1)
	go func() {
		defer a.manualRuns.Done()
		result, err := a.runManualSearch(runCtx, target.mediaID, target.media, target.language, manualOptions{jobID: target.jobID})
		done <- finished{result, err}
	}()
	result := control.RejectResult{MediaID: target.mediaID, Rejected: control.LanguageOutcome{Language: target.language.String(), Outcome: "running"}}
	if wait == 0 {
		return result, attrs, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case item := <-done:
		if item.err != nil {
			if runCtx.Err() != nil {
				return control.RejectResult{}, nil, &control.Error{Code: control.CodeDaemonStopping, Message: "subsyncd is shutting down; the rejection stands and its search stays queued"}
			}
			return control.RejectResult{}, nil, item.err
		}
		result.Rejected = a.languageOutcome(target.language, item.result)
	case <-timer.C:
		// The search carries on in the daemon.
	case <-runCtx.Done():
		return control.RejectResult{}, nil, &control.Error{Code: control.CodeDaemonStopping, Message: "subsyncd is shutting down; the rejection stands and its search stays queued"}
	}
	return result, attrs, nil
}

// prepareReject validates the arguments and finds the managed subtitle. It
// changes nothing. Only a subtitle subsyncd installed, inside a media root,
// whose file still matches the recorded checksum, can be rejected; embedded
// tracks, the owner's own sidecars and edited managed sidecars are
// not_managed.
func (a *App) prepareReject(ctx context.Context, args control.RejectArgs) (rejectTarget, error) {
	switch {
	case args.Undo && args.Reason != "":
		return rejectTarget{}, &control.Error{Code: control.CodeUsage, Message: "--reason is not used with --undo"}
	case !args.Undo && args.Reason == "":
		return rejectTarget{}, &control.Error{Code: control.CodeUsage, Message: "--reason is required: one of " + strings.Join(control.RejectReasons, ", ")}
	case !args.Undo && !control.ValidRejectReason(args.Reason):
		return rejectTarget{}, &control.Error{Code: control.CodeUsage, Message: fmt.Sprintf("unknown --reason %q: use one of %s", args.Reason, strings.Join(control.RejectReasons, ", "))}
	}
	var named domain.Language
	if args.Language != "" {
		language, _, err := a.workflowFor(args.Language)
		if err != nil {
			return rejectTarget{}, err
		}
		named = language
	}
	mediaID, media, err := a.Resolve(ctx, args.Selection)
	if err != nil {
		return rejectTarget{}, err
	}
	target := rejectTarget{mediaID: mediaID, media: media, language: named, reason: args.Reason, undo: args.Undo}
	if target.language == "" {
		if target.language, err = a.rejectLanguage(ctx, mediaID, args.Undo); err != nil {
			return rejectTarget{}, err
		}
	}
	if args.Undo {
		return target, nil
	}
	installation, found, err := a.Repository.GetInstallation(ctx, mediaID, target.language)
	if err != nil {
		return rejectTarget{}, err
	}
	if !found {
		return rejectTarget{}, &control.Error{Code: control.CodeNotManaged, Message: fmt.Sprintf("subsyncd installed no %s subtitle for this file; embedded tracks and your own sidecars are not managed", target.language)}
	}
	if err := managedSubtitleUnchanged(installation, a.Config.MediaRoots); err != nil {
		return rejectTarget{}, err
	}
	target.installation = installation
	if target.jobID, err = manualJobID(); err != nil {
		return rejectTarget{}, err
	}
	return target, nil
}

// rejectLanguage picks the language when none was named: the only managed
// one. For --undo it is the only configured language.
func (a *App) rejectLanguage(ctx context.Context, mediaID int64, undo bool) (domain.Language, error) {
	if undo {
		configured := sortedLanguages(a.Config)
		if len(configured) == 1 {
			return configured[0], nil
		}
		return "", &control.Error{Code: control.CodeUsage, Message: "--language is required with --undo when several languages are configured"}
	}
	installed, err := a.Repository.InstalledLanguages(ctx, mediaID)
	if err != nil {
		return "", err
	}
	switch len(installed) {
	case 0:
		return "", &control.Error{Code: control.CodeNotManaged, Message: "subsyncd installed no subtitle for this file; embedded tracks and your own sidecars are not managed"}
	case 1:
		return installed[0], nil
	}
	names := make([]string, len(installed))
	for i, language := range installed {
		names[i] = language.String()
	}
	return "", &control.Error{Code: control.CodeUsage, Message: fmt.Sprintf("--language is required: this file has managed subtitles in %s", strings.Join(names, ", "))}
}

// managedSubtitleUnchanged checks that the installed subtitle is a regular
// file inside a media root whose content still has the recorded checksum.
func managedSubtitleUnchanged(installation store.Installation, roots []string) error {
	notManaged := &control.Error{Code: control.CodeNotManaged, Message: "the subtitle changed on disk since subsyncd installed it, so it counts as yours and is left alone"}
	if !withinRoots(installation.Path, roots) {
		return &control.Error{Code: control.CodeNotManaged, Message: "the installed subtitle is outside the configured media roots"}
	}
	info, err := os.Lstat(installation.Path)
	if errors.Is(err, os.ErrNotExist) {
		return &control.Error{Code: control.CodeNotManaged, Message: "the installed subtitle is no longer on disk"}
	}
	if err != nil {
		return fmt.Errorf("inspect installed subtitle: %w", err)
	}
	if !info.Mode().IsRegular() {
		return notManaged
	}
	payload, err := os.ReadFile(installation.Path)
	if err != nil {
		return fmt.Errorf("read installed subtitle: %w", err)
	}
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != installation.Checksum {
		return notManaged
	}
	return nil
}

// recordReject is the durable step: the rejection, bound to the media file
// the subtitle was installed for, and the follow-up search (due now, import
// priority, replace_installed, leased to the manual run) commit together. A
// restart before the search finishes leaves both, and whichever run takes
// the search still replaces or removes the subtitle.
func (a *App) recordReject(ctx context.Context, target rejectTarget) error {
	_, service, err := a.workflowFor(target.language.String())
	if err != nil {
		return err
	}
	installation := target.installation
	// An installation from before candidate signatures were recorded is
	// rejected by provider and result ID: the same result in any form.
	signature := installation.CandidateSignature
	if signature == "" {
		signature = store.AnyCandidateSignature
	}
	now := a.Clock.Now()
	rejection := store.CandidateRejection{
		MediaID: target.mediaID, Language: target.language.String(), ProviderID: installation.ProviderID, ResultID: installation.CandidateID,
		CandidateSignature: signature, ArtifactChecksum: installation.ArtifactChecksum, ReasonCode: store.UserRejectionPrefix + target.reason, ToolSignature: service.RejectionToolSignature(),
		MediaPath: installation.MediaPath, MediaFileID: installation.MediaFileID, MediaSize: installation.MediaSize, MediaModTimeNS: installation.MediaModTimeNS, RejectedAt: now,
	}
	if err := a.Repository.RecordUserRejection(ctx, rejection, now, target.jobID, manualLeaseDuration, store.SearchHistoryDetail{MediaTitle: observability.MediaTitle(target.media), ProviderID: installation.ProviderID}); err != nil {
		return err
	}
	events := a.events().For("app")
	events.Log(observability.WithAttrs(ctx, slog.Int64("media_id", target.mediaID), slog.String("language", target.language.String())), slog.LevelInfo, "candidate.rejected", "subtitle candidate rejected",
		slog.String("provider", observability.SafeText(installation.ProviderID)), slog.String("candidate_id", observability.SafeText(installation.CandidateID)), slog.String("reason_code", rejection.ReasonCode))
	return nil
}

// undoReject removes the owner's rejections for the file and language. It
// runs no search.
func (a *App) undoReject(ctx context.Context, target rejectTarget) (control.RejectResult, error) {
	undone, err := a.Repository.ClearUserRejections(ctx, target.mediaID, target.language)
	if err != nil {
		return control.RejectResult{}, err
	}
	return control.RejectResult{MediaID: target.mediaID, Rejected: control.LanguageOutcome{Language: target.language.String()}, Undone: int(undone), Undo: true}, nil
}
