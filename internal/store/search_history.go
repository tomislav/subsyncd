package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"subsyncd/internal/domain"
)

// HistoryRetention is how long finished searches stay in the history.
const HistoryRetention = 30 * 24 * time.Hour

// SearchHistoryDetail is what a caller knows about a finished search beyond
// what the store reads from the media row. It holds no provider text or paths.
type SearchHistoryDetail struct {
	MediaTitle  string
	ProviderID  string
	ReleaseName string
	Reason      string
	Manual      bool
}

// HistoryEntry is one finished search. MediaID is zero once the media row is
// gone; the entry keeps its reference and title.
type HistoryEntry struct {
	MediaID     int64
	Ref         domain.MediaRef
	MediaTitle  string
	Language    domain.Language
	Outcome     string
	ProviderID  string
	ReleaseName string
	Reason      string
	Manual      bool
	FinishedAt  time.Time
}

// HistoryQuery selects finished searches. Outcomes, when set, keeps only
// those outcomes; Limit applies after every filter.
type HistoryQuery struct {
	Since    time.Time
	Instance string
	Language domain.Language
	Outcomes []string
	Limit    int
}

// ProviderCounts tallies history outcomes that name one provider.
type ProviderCounts struct{ Installed, NoResult, Rejected int }

type historyExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertSearchHistory(ctx context.Context, executor historyExecer, mediaID int64, language domain.Language, outcome string, detail SearchHistoryDetail, at time.Time) error {
	result, err := executor.ExecContext(ctx, `INSERT INTO search_history(media_id, instance, kind, file_id, media_title, language, outcome, provider_id, release_name, reason, manual, finished_at_ns)
 SELECT id, instance, kind, file_id, ?, ?, ?, ?, ?, ?, ?, ? FROM media WHERE id=?`,
		detail.MediaTitle, language.String(), outcome, detail.ProviderID, detail.ReleaseName, detail.Reason, detail.Manual, at.UnixNano(), mediaID)
	if err != nil {
		return fmt.Errorf("record search history: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return fmt.Errorf("record search history: media %d not found", mediaID)
	}
	return nil
}

// RecordSearchHistory appends one finished search for a manual run. Worker
// completions write theirs inside CompleteSearch instead.
func (r *Repository) RecordSearchHistory(ctx context.Context, mediaID int64, language domain.Language, outcome string, detail SearchHistoryDetail, at time.Time) error {
	return insertSearchHistory(ctx, r.store.db, mediaID, language, outcome, detail, at)
}

// ListHistory returns finished searches, newest first.
func (r *Repository) ListHistory(ctx context.Context, q HistoryQuery) ([]HistoryEntry, error) {
	var where []string
	var args []any
	if !q.Since.IsZero() {
		where = append(where, "finished_at_ns >= ?")
		args = append(args, q.Since.UnixNano())
	}
	if q.Instance != "" {
		where = append(where, "instance = ?")
		args = append(args, q.Instance)
	}
	if q.Language != "" {
		where = append(where, "language = ?")
		args = append(args, q.Language.String())
	}
	if len(q.Outcomes) > 0 {
		where = append(where, "outcome IN ("+strings.TrimSuffix(strings.Repeat("?,", len(q.Outcomes)), ",")+")")
		for _, outcome := range q.Outcomes {
			args = append(args, outcome)
		}
	}
	query := `SELECT media_id, instance, kind, file_id, media_title, language, outcome, provider_id, release_name, reason, manual, finished_at_ns FROM search_history`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY finished_at_ns DESC, id DESC"
	if q.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, q.Limit)
	}
	rows, err := r.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list search history: %w", err)
	}
	defer rows.Close()
	var entries []HistoryEntry
	for rows.Next() {
		var entry HistoryEntry
		var mediaID sql.NullInt64
		var kind, language string
		var finished int64
		if err := rows.Scan(&mediaID, &entry.Ref.Instance, &kind, &entry.Ref.FileID, &entry.MediaTitle, &language, &entry.Outcome, &entry.ProviderID, &entry.ReleaseName, &entry.Reason, &entry.Manual, &finished); err != nil {
			return nil, fmt.Errorf("scan search history: %w", err)
		}
		entry.MediaID = mediaID.Int64
		entry.Ref.Kind = domain.MediaKind(kind)
		entry.Language = domain.Language(language)
		entry.FinishedAt = time.Unix(0, finished).UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read search history: %w", err)
	}
	return entries, nil
}

// PruneHistory deletes entries finished before the cutoff.
func (r *Repository) PruneHistory(ctx context.Context, before time.Time) (int64, error) {
	result, err := r.store.db.ExecContext(ctx, `DELETE FROM search_history WHERE finished_at_ns < ?`, before.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("prune search history: %w", err)
	}
	return result.RowsAffected()
}

// ProviderHistoryCounts tallies installed, no_result and rejected searches
// since the given time, by the provider each entry names.
func (r *Repository) ProviderHistoryCounts(ctx context.Context, since time.Time) (map[string]ProviderCounts, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT provider_id, outcome, count(*) FROM search_history WHERE provider_id <> '' AND finished_at_ns >= ? AND outcome IN ('installed', 'no_result', 'rejected') GROUP BY provider_id, outcome`, since.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("count search history: %w", err)
	}
	defer rows.Close()
	counts := map[string]ProviderCounts{}
	for rows.Next() {
		var provider, outcome string
		var n int
		if err := rows.Scan(&provider, &outcome, &n); err != nil {
			return nil, fmt.Errorf("scan search history counts: %w", err)
		}
		c := counts[provider]
		switch outcome {
		case "installed":
			c.Installed = n
		case "no_result":
			c.NoResult = n
		case "rejected":
			c.Rejected = n
		}
		counts[provider] = c
	}
	return counts, rows.Err()
}
