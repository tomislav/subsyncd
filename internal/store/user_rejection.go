package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"subsyncd/internal/domain"
)

// UserRejectionPrefix starts the reason code of a rejection the owner made
// with reject, as opposed to one subsyncd made itself.
const UserRejectionPrefix = "user_"

// AnyCandidateSignature is the candidate signature of a user rejection of an
// installation that predates recorded candidate signatures. It matches the
// provider result in any form.
const AnyCandidateSignature = "*"

// userReasonSQL matches a user rejection's reason code. The underscore is a
// LIKE wildcard, so it is escaped.
const userReasonSQL = `reason_code LIKE 'user\_%' ESCAPE '\'`

// IsUserRejection reports whether reasonCode is the owner's.
func IsUserRejection(reasonCode string) bool {
	return strings.HasPrefix(reasonCode, UserRejectionPrefix)
}

// RecordUserRejection records the owner's rejection of an installed candidate
// and queues the follow-up search in one transaction: the search becomes due
// at now at import priority, with replace_installed set so whichever run
// takes it treats the rejected subtitle as replaceable or removable. A search
// leased at now gets a rerun request instead, so that run's completion leaves
// it queued.
func (r *Repository) RecordUserRejection(ctx context.Context, rejection CandidateRejection, now time.Time) error {
	if err := validateCandidateRejection(rejection); err != nil {
		return err
	}
	if !IsUserRejection(rejection.ReasonCode) {
		return fmt.Errorf("user rejection reason must start with %q", UserRejectionPrefix)
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin user rejection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := putCandidateRejection(ctx, tx, rejection); err != nil {
		return err
	}
	at := now.UnixNano()
	if _, err := tx.ExecContext(ctx, `INSERT INTO search_states(media_id, language, state, attempt, failure_attempt, next_attempt_at_ns, queue_order_ns, priority, replace_installed)
 VALUES (?, ?, 'pending', 0, 0, ?, ?, ?, 1)
 ON CONFLICT(media_id, language) DO UPDATE SET state='pending', attempt=0, failure_attempt=0, next_attempt_at_ns=excluded.next_attempt_at_ns,
 queue_order_ns=excluded.queue_order_ns, priority=excluded.priority, last_outcome='', replace_installed=1,
 resume_providers_json='[]', resume_route_signature='', resume_saved_at_ns=0,
 rerun_requested=CASE WHEN search_states.lease_until_ns IS NOT NULL AND search_states.lease_until_ns > ? THEN 1 ELSE search_states.rerun_requested END`,
		rejection.MediaID, rejection.Language, at, at, SearchPriorityImport, at); err != nil {
		return fmt.Errorf("queue rejected subtitle replacement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit user rejection: %w", err)
	}
	return nil
}

// ClearUserRejections removes the owner's rejections for a media file and
// language, for reject --undo, and reports how many it removed.
func (r *Repository) ClearUserRejections(ctx context.Context, mediaID int64, language domain.Language) (int64, error) {
	result, err := r.store.db.ExecContext(ctx, `DELETE FROM candidate_rejections WHERE media_id=? AND language=? AND `+userReasonSQL, mediaID, language.String())
	if err != nil {
		return 0, fmt.Errorf("clear user rejections: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count cleared user rejections: %w", err)
	}
	return count, nil
}

// InstalledLanguages lists the languages a media file has an installation
// record for, sorted.
func (r *Repository) InstalledLanguages(ctx context.Context, mediaID int64) ([]domain.Language, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT language FROM installations WHERE media_id=? ORDER BY language`, mediaID)
	if err != nil {
		return nil, fmt.Errorf("list installed languages: %w", err)
	}
	defer rows.Close()
	var languages []domain.Language
	for rows.Next() {
		var language string
		if err := rows.Scan(&language); err != nil {
			return nil, fmt.Errorf("scan installed language: %w", err)
		}
		languages = append(languages, domain.Language(language))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate installed languages: %w", err)
	}
	return languages, nil
}

// InstallationRemoval removes one managed subtitle's record. Installation is
// the record as read; it is removed only while it is still exactly that.
type InstallationRemoval struct {
	Installation  Installation
	History       SearchHistoryDetail
	At            time.Time
	Notifications []NotificationRequest
}

// RemoveInstallation deletes an installation record, records the audit event
// and a search history row with outcome removed, and queues the notification
// intents, all in one transaction. It fails, changing nothing, when the record
// is no longer the one read.
func (r *Repository) RemoveInstallation(ctx context.Context, removal InstallationRemoval) ([]NotificationEnqueueResult, error) {
	for _, request := range removal.Notifications {
		if err := validateNotificationRequest(request); err != nil {
			return nil, err
		}
	}
	installation := removal.Installation
	at := removal.At
	if at.IsZero() {
		at = time.Now()
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin installation removal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `DELETE FROM installations WHERE media_id=? AND language=? AND path=? AND checksum=? AND provider_id=? AND candidate_id=? AND rollback_path=?`,
		installation.MediaID, installation.Language, installation.Path, installation.Checksum, installation.ProviderID, installation.CandidateID, installation.RollbackPath)
	if err != nil {
		return nil, fmt.Errorf("remove installation: %w", err)
	}
	if err := requireOneRow(result, "installation changed before removal"); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(event_type, media_id, outcome, created_at_ns) VALUES ('subtitle_removed', ?, 'success', ?)`, installation.MediaID, at.UnixNano()); err != nil {
		return nil, fmt.Errorf("record removal audit: %w", err)
	}
	if err := insertSearchHistory(ctx, tx, installation.MediaID, domain.Language(installation.Language), "removed", removal.History, at); err != nil {
		return nil, err
	}
	results := make([]NotificationEnqueueResult, 0, len(removal.Notifications))
	for _, request := range removal.Notifications {
		inserted, err := enqueueNotification(ctx, tx, request)
		if err != nil {
			return nil, err
		}
		results = append(results, NotificationEnqueueResult{Notifier: request.Notifier, DedupeKey: request.DedupeKey, Inserted: inserted})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit installation removal: %w", err)
	}
	return results, nil
}
