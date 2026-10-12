package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"subsyncd/internal/catalog"
	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/selection"
	"subsyncd/internal/store"
)

// maxAmbiguousCandidates bounds the candidates an ambiguous_media error lists,
// so a series name without an episode does not return a whole library.
const maxAmbiguousCandidates = 25

func selectionUsage(message string) error {
	return &control.Error{Code: control.CodeUsage, Message: message}
}

// matchSelection returns the media a selection names: the one file for
// --file-id, otherwise every active file matching --title or --path.
func (a *App) matchSelection(ctx context.Context, sel control.Selection) ([]control.MediaMatch, error) {
	forms := 0
	if sel.FileID != 0 {
		forms++
	}
	if sel.Title != "" {
		forms++
	}
	if sel.Path != "" {
		forms++
	}
	switch {
	case forms == 0:
		return nil, selectionUsage("select media with --file-id, --title or --path")
	case forms > 1:
		return nil, selectionUsage("use only one of --file-id, --title and --path")
	}
	if sel.Kind != "" && sel.Kind != string(domain.MediaMovie) && sel.Kind != string(domain.MediaEpisode) {
		return nil, selectionUsage("kind must be movie or episode")
	}
	if sel.FileID != 0 {
		if sel.Instance == "" || sel.Kind == "" || sel.FileID < 0 {
			return nil, selectionUsage("--file-id needs --instance and --kind")
		}
	}
	if sel.Instance != "" && a.Catalogs[sel.Instance] == nil {
		return nil, unknownInstance(sel.Instance)
	}
	switch {
	case sel.FileID != 0:
		_, media, err := a.Repository.FindMedia(ctx, domain.MediaRef{Instance: sel.Instance, Kind: domain.MediaKind(sel.Kind), FileID: sel.FileID})
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &control.Error{Code: control.CodeMediaNotFound, Message: "media is not indexed"}
		}
		if err != nil {
			return nil, err
		}
		return []control.MediaMatch{matchOf(media)}, nil
	case sel.Title != "":
		query, err := selection.ParseTitle(sel.Title)
		if err != nil {
			return nil, selectionUsage(err.Error())
		}
		return a.Repository.MatchMedia(ctx, store.MediaQuery{Instance: sel.Instance, Kind: sel.Kind, Title: query})
	}
	return a.matchPath(ctx, sel)
}

// matchPath matches the stored path exactly. The path may be a path inside
// this container or one as Sonarr or Radarr reports it, which each configured
// instance's path mappings translate.
func (a *App) matchPath(ctx context.Context, sel control.Selection) ([]control.MediaMatch, error) {
	paths := []string{filepath.Clean(sel.Path)}
	for _, instance := range a.Config.Instances {
		if sel.Instance != "" && instance.Name != sel.Instance {
			continue
		}
		if mapped, err := catalog.MapPath(sel.Path, instance.PathMappings, a.Config.MediaRoots); err == nil {
			paths = append(paths, mapped)
		}
	}
	seen := map[control.MediaMatch]bool{}
	var matches []control.MediaMatch
	for _, path := range paths {
		found, err := a.Repository.MatchMedia(ctx, store.MediaQuery{Instance: sel.Instance, Kind: sel.Kind, Path: path})
		if err != nil {
			return nil, err
		}
		for _, match := range found {
			if !seen[match] {
				seen[match] = true
				matches = append(matches, match)
			}
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Instance != matches[j].Instance {
			return matches[i].Instance < matches[j].Instance
		}
		return matches[i].FileID < matches[j].FileID
	})
	return matches, nil
}

func matchOf(media domain.Media) control.MediaMatch {
	return control.MediaMatch{Instance: media.Ref.Instance, Kind: string(media.Ref.Kind), FileID: media.Ref.FileID, Title: media.Title, Year: media.Year, Season: media.Season, Episode: media.Episode, EpisodeEnd: media.EpisodeEnd}
}

// Resolve turns a selection into exactly one indexed media file. Several
// matches are an ambiguous_media error listing them; none is media_not_found.
func (a *App) Resolve(ctx context.Context, sel control.Selection) (int64, domain.Media, error) {
	matches, err := a.matchSelection(ctx, sel)
	if err != nil {
		return 0, domain.Media{}, err
	}
	switch len(matches) {
	case 0:
		return 0, domain.Media{}, &control.Error{Code: control.CodeMediaNotFound, Message: "no indexed media matches this selection; check it with find"}
	case 1:
		return a.Repository.FindMedia(ctx, domain.MediaRef{Instance: matches[0].Instance, Kind: domain.MediaKind(matches[0].Kind), FileID: matches[0].FileID})
	}
	message := fmt.Sprintf("%d files match this selection; narrow it with --instance, --kind, a year or an episode, or use --file-id", len(matches))
	if len(matches) > maxAmbiguousCandidates {
		matches = matches[:maxAmbiguousCandidates]
		message += fmt.Sprintf(" (first %d listed)", maxAmbiguousCandidates)
	}
	return 0, domain.Media{}, &control.Error{Code: control.CodeAmbiguousMedia, Message: message, Candidates: matches}
}

// Find lists the media a selection matches, each with the state of every
// configured language. No match is an empty list, not an error.
func (a *App) Find(ctx context.Context, sel control.Selection) (control.FindResult, error) {
	matches, err := a.matchSelection(ctx, sel)
	if err != nil {
		return control.FindResult{}, err
	}
	now := a.Clock.Now()
	languages := sortedLanguages(a.Config)
	result := control.FindResult{Matches: make([]control.FindMatch, 0, len(matches))}
	for _, match := range matches {
		mediaID, _, err := a.Repository.FindMedia(ctx, domain.MediaRef{Instance: match.Instance, Kind: domain.MediaKind(match.Kind), FileID: match.FileID})
		if err != nil {
			return control.FindResult{}, err
		}
		states, err := a.Repository.LanguageStates(ctx, mediaID, now)
		if err != nil {
			return control.FindResult{}, err
		}
		found := control.FindMatch{MediaMatch: match, Languages: make([]control.LanguageState, 0, len(languages))}
		for _, language := range languages {
			state := states[language.String()]
			if state == "" {
				state = "missing"
			}
			found.Languages = append(found.Languages, control.LanguageState{Language: language.String(), State: state})
		}
		result.Matches = append(result.Matches, found)
	}
	return result, nil
}
