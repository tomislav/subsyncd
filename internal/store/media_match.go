package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"subsyncd/internal/control"
	"subsyncd/internal/selection"
)

// MediaQuery selects active media by title or by exact stored path. Instance
// and Kind narrow either form when set.
type MediaQuery struct {
	Instance, Kind string
	Title          selection.TitleQuery
	Path           string
}

// MatchMedia returns the active media matching the query, ordered by instance,
// kind, title, season, episode and file ID. A title query returns only the best
// rank: an exact normalized title or alternate title beats a substring match.
// SQL narrows by the exact columns; titles are normalized and ranked in Go
// because SQLite's lower() folds only ASCII.
func (r *Repository) MatchMedia(ctx context.Context, q MediaQuery) ([]control.MediaMatch, error) {
	where := []string{"deleted=0"}
	var args []any
	if q.Instance != "" {
		where = append(where, "instance=?")
		args = append(args, q.Instance)
	}
	if q.Kind != "" {
		where = append(where, "kind=?")
		args = append(args, q.Kind)
	}
	wantTitle := ""
	if q.Title.Name != "" {
		wantTitle = selection.Normalize(q.Title.Name)
		if wantTitle == "" {
			return nil, nil
		}
		if q.Title.Year > 0 {
			where = append(where, "year=?")
			args = append(args, q.Title.Year)
		}
		if q.Title.Season > 0 || q.Title.Episode > 0 {
			where = append(where, "season=? AND (episode=? OR (episode<=? AND episode_end>=?))")
			args = append(args, q.Title.Season, q.Title.Episode, q.Title.Episode, q.Title.Episode)
		}
	} else if q.Path != "" {
		where = append(where, "path=?")
		args = append(args, q.Path)
	} else {
		return nil, fmt.Errorf("match media: title or path is required")
	}
	rows, err := r.store.db.QueryContext(ctx, `SELECT instance, kind, file_id, title, alternate_titles_json, year, season, episode, episode_end FROM media WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		return nil, fmt.Errorf("match media: %w", err)
	}
	defer rows.Close()
	type ranked struct {
		match control.MediaMatch
		key   string
		rank  int
	}
	var found []ranked
	best := 2
	for rows.Next() {
		var item ranked
		var alternates []byte
		m := &item.match
		if err := rows.Scan(&m.Instance, &m.Kind, &m.FileID, &m.Title, &alternates, &m.Year, &m.Season, &m.Episode, &m.EpisodeEnd); err != nil {
			return nil, fmt.Errorf("scan media match: %w", err)
		}
		item.key = selection.Normalize(m.Title)
		if wantTitle != "" {
			var titles []string
			if err := json.Unmarshal(alternates, &titles); err != nil {
				return nil, fmt.Errorf("decode alternate titles: %w", err)
			}
			item.rank = 2
			for _, title := range append([]string{m.Title}, titles...) {
				normalized := selection.Normalize(title)
				switch {
				case normalized == wantTitle:
					item.rank = 0
				case strings.Contains(normalized, wantTitle):
					item.rank = min(item.rank, 1)
				}
			}
			if item.rank == 2 {
				continue
			}
			best = min(best, item.rank)
		}
		found = append(found, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate media matches: %w", err)
	}
	var matches []ranked
	for _, item := range found {
		if wantTitle == "" || item.rank == best {
			matches = append(matches, item)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		switch {
		case a.match.Instance != b.match.Instance:
			return a.match.Instance < b.match.Instance
		case a.match.Kind != b.match.Kind:
			return a.match.Kind < b.match.Kind
		case a.key != b.key:
			return a.key < b.key
		case a.match.Year != b.match.Year:
			return a.match.Year < b.match.Year
		case a.match.Season != b.match.Season:
			return a.match.Season < b.match.Season
		case a.match.Episode != b.match.Episode:
			return a.match.Episode < b.match.Episode
		}
		return a.match.FileID < b.match.FileID
	})
	out := make([]control.MediaMatch, len(matches))
	for i, item := range matches {
		out[i] = item.match
	}
	return out, nil
}

// LanguageStates reports, per language, which of "running", "installed" or
// "satisfied" applies to a media file at now, in that order of precedence. A
// language with none of them is absent. Running means a live search lease;
// installed means an installation record; satisfied means the last search
// found an existing track or sidecar already covers the language.
func (r *Repository) LanguageStates(ctx context.Context, mediaID int64, now time.Time) (map[string]string, error) {
	states := map[string]string{}
	rows, err := r.store.db.QueryContext(ctx, `SELECT language FROM installations WHERE media_id=?`, mediaID)
	if err != nil {
		return nil, fmt.Errorf("list installed languages: %w", err)
	}
	for rows.Next() {
		var language string
		if err := rows.Scan(&language); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan installed language: %w", err)
		}
		states[language] = "installed"
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close installed languages: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate installed languages: %w", err)
	}
	rows, err = r.store.db.QueryContext(ctx, `SELECT language, last_outcome, COALESCE(lease_until_ns, 0) FROM search_states WHERE media_id=?`, mediaID)
	if err != nil {
		return nil, fmt.Errorf("list search languages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var language, outcome string
		var leaseUntil int64
		if err := rows.Scan(&language, &outcome, &leaseUntil); err != nil {
			return nil, fmt.Errorf("scan search language: %w", err)
		}
		switch {
		case leaseUntil > now.UnixNano():
			states[language] = "running"
		case states[language] == "" && outcome == "satisfied":
			states[language] = "satisfied"
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search languages: %w", err)
	}
	return states, nil
}
