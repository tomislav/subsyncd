package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/selection"
)

// MissingQuery selects the configured languages an active media file has no
// subtitle for. Languages are the configured languages; Language, Instance and
// Title narrow them. Limit zero or less returns every row.
type MissingQuery struct {
	Instance string
	Language domain.Language
	Title    selection.TitleQuery
	Limit    int
	// Languages lists the configured languages; AllowHearingImpaired is the
	// configured policy for whether a hearing-impaired track satisfies one.
	Languages            []domain.Language
	AllowHearingImpaired bool
	// Now decides whether a search lease is live.
	Now time.Time
}

// MissingRow is one media file and language that has no installation and no
// satisfying track, with what the search schedule recorded about it.
type MissingRow struct {
	control.MediaMatch
	MediaID int64
	// Path is the stored media path, for the caller to check; it is not output.
	Path              string
	Language          string
	UnsupportedReason string
	// NoSearch is set when no search state exists for the language yet.
	NoSearch       bool
	State          string
	Attempt        int
	FailureAttempt int
	LastOutcome    string
	NextAttemptAt  time.Time
	// Running is set while a live lease holds the search.
	Running bool
}

// ListMissing returns the rows in queue order (priority, instance rank, queue
// time and media order, as the worker leases them; pending searches before
// finished ones, rows with no search state last) and the total before Limit.
// A language is not missing when it has an installation, its last search found
// it satisfied, or a track covers it (domain.TrackCoversLanguage). The title
// filter is MatchMedia's.
func (r *Repository) ListMissing(ctx context.Context, q MissingQuery) ([]MissingRow, int, error) {
	languages := q.Languages
	if q.Language != "" {
		languages = []domain.Language{q.Language}
	}
	if len(languages) == 0 {
		return nil, 0, nil
	}
	var allowed map[control.MediaMatch]bool
	if q.Title.Name != "" {
		matches, err := r.MatchMedia(ctx, MediaQuery{Instance: q.Instance, Title: q.Title})
		if err != nil {
			return nil, 0, err
		}
		if len(matches) == 0 {
			return nil, 0, nil
		}
		allowed = make(map[control.MediaMatch]bool, len(matches))
		for _, match := range matches {
			allowed[match] = true
		}
	}
	args := make([]any, 0, len(languages)+1)
	for _, language := range languages {
		args = append(args, language.String())
	}
	where := ""
	if q.Instance != "" {
		where = " AND m.instance=?"
		args = append(args, q.Instance)
	}
	rows, err := r.store.db.QueryContext(ctx, `WITH wanted(language) AS (VALUES `+strings.TrimSuffix(strings.Repeat("(?),", len(languages)), ",")+`)
SELECT m.id, m.path, m.instance, m.kind, m.file_id, m.title, m.year, m.season, m.episode, m.episode_end, m.unsupported_reason,
  w.language, s.id IS NULL, COALESCE(s.state, ''), COALESCE(s.attempt, 0), COALESCE(s.failure_attempt, 0), COALESCE(s.last_outcome, ''), COALESCE(s.next_attempt_at_ns, 0), COALESCE(s.lease_until_ns, 0)
FROM media m CROSS JOIN wanted w
LEFT JOIN search_states s ON s.media_id=m.id AND s.language=w.language
WHERE m.deleted=0`+where+`
  AND COALESCE(s.last_outcome, '')<>'satisfied'
  AND NOT EXISTS (SELECT 1 FROM installations i WHERE i.media_id=m.id AND i.language=w.language)
ORDER BY s.id IS NULL, COALESCE(s.state, '')<>'pending', s.priority DESC, s.instance_rank DESC, s.queue_order_ns, s.next_attempt_at_ns,
  s.sort_title, s.sort_series, s.sort_season, s.sort_episode, m.id, w.language`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list missing: %w", err)
	}
	defer rows.Close()
	var candidates []MissingRow
	for rows.Next() {
		var row MissingRow
		var next, lease int64
		if err := rows.Scan(&row.MediaID, &row.Path, &row.Instance, &row.Kind, &row.FileID, &row.Title, &row.Year, &row.Season, &row.Episode, &row.EpisodeEnd, &row.UnsupportedReason,
			&row.Language, &row.NoSearch, &row.State, &row.Attempt, &row.FailureAttempt, &row.LastOutcome, &next, &lease); err != nil {
			return nil, 0, fmt.Errorf("scan missing: %w", err)
		}
		if allowed != nil && !allowed[row.MediaMatch] {
			continue
		}
		row.NextAttemptAt = fromUnixNano(next)
		row.Running = lease > q.Now.UnixNano()
		candidates = append(candidates, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate missing: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("close missing: %w", err)
	}
	tracks, err := r.candidateTracks(ctx)
	if err != nil {
		return nil, 0, err
	}
	var missing []MissingRow
	for _, row := range candidates {
		if !covered(tracks[row.MediaID], domain.Language(row.Language), q.AllowHearingImpaired) {
			missing = append(missing, row)
		}
	}
	total := len(missing)
	if q.Limit > 0 && len(missing) > q.Limit {
		missing = missing[:q.Limit]
	}
	return missing, total, nil
}

type trackKind struct {
	language    domain.Language
	forced, sdh bool
}

func covered(tracks []trackKind, language domain.Language, allowHearingImpaired bool) bool {
	for _, track := range tracks {
		if domain.TrackCoversLanguage(track.language, track.forced, track.sdh, language, allowHearingImpaired) {
			return true
		}
	}
	return false
}

// candidateTracks lists, per media file, the language, forced and
// hearing-impaired flags of every track.
func (r *Repository) candidateTracks(ctx context.Context) (map[int64][]trackKind, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT media_id, language, forced, sdh FROM tracks WHERE language<>''`)
	if err != nil {
		return nil, fmt.Errorf("list tracks: %w", err)
	}
	defer rows.Close()
	tracks := map[int64][]trackKind{}
	for rows.Next() {
		var mediaID int64
		var language string
		var track trackKind
		if err := rows.Scan(&mediaID, &language, &track.forced, &track.sdh); err != nil {
			return nil, fmt.Errorf("scan track: %w", err)
		}
		track.language = domain.Language(language)
		tracks[mediaID] = append(tracks[mediaID], track)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tracks: %w", err)
	}
	return tracks, nil
}
