package store

import (
	"context"
	"fmt"
)

// InstanceLanguage keys a count by instance and subtitle language.
type InstanceLanguage struct{ Instance, Language string }

// CoverageSnapshot is what coverage counts, read from one database snapshot
// so the counts agree with each other while the daemon writes: the active
// media files of each instance, the active files with an installation per
// instance and language, and the rows ListMissing returns for the query.
type CoverageSnapshot struct {
	Media     map[string]int
	Installed map[InstanceLanguage]int
	Missing   []MissingRow
}

// Coverage reads a CoverageSnapshot in one read transaction. The query's
// Title and Limit are ignored: coverage counts every row.
func (r *Repository) Coverage(ctx context.Context, q MissingQuery) (CoverageSnapshot, error) {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return CoverageSnapshot{}, fmt.Errorf("begin coverage read: %w", err)
	}
	// The transaction only reads, so rolling it back is how it ends.
	defer func() { _ = tx.Rollback() }()
	var snapshot CoverageSnapshot
	if snapshot.Media, err = activeMediaCounts(ctx, tx); err != nil {
		return CoverageSnapshot{}, err
	}
	if snapshot.Installed, err = installedCounts(ctx, tx); err != nil {
		return CoverageSnapshot{}, err
	}
	if languages := q.languages(); len(languages) > 0 {
		q.Limit = 0
		if snapshot.Missing, _, err = listMissing(ctx, tx, q, languages, nil); err != nil {
			return CoverageSnapshot{}, err
		}
	}
	return snapshot, nil
}

// activeMediaCounts counts the active media files of each instance.
func activeMediaCounts(ctx context.Context, db querier) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT instance, count(*) FROM media WHERE deleted=0 GROUP BY instance`)
	if err != nil {
		return nil, fmt.Errorf("count media: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var instance string
		var count int
		if err := rows.Scan(&instance, &count); err != nil {
			return nil, fmt.Errorf("scan media count: %w", err)
		}
		counts[instance] = count
	}
	return counts, rows.Err()
}

// installedCounts counts, per instance and language, the active media files
// that have an installation: the files ListMissing leaves out for having one.
func installedCounts(ctx context.Context, db querier) (map[InstanceLanguage]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.instance, i.language, count(DISTINCT m.id)
FROM installations i JOIN media m ON m.id=i.media_id WHERE m.deleted=0 GROUP BY m.instance, i.language`)
	if err != nil {
		return nil, fmt.Errorf("count installations: %w", err)
	}
	defer rows.Close()
	counts := map[InstanceLanguage]int{}
	for rows.Next() {
		var key InstanceLanguage
		var count int
		if err := rows.Scan(&key.Instance, &key.Language, &count); err != nil {
			return nil, fmt.Errorf("scan installation count: %w", err)
		}
		counts[key] = count
	}
	return counts, rows.Err()
}
