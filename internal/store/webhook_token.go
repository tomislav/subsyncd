package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrNoWebhookToken reports that an instance exists but has no generated
// webhook token yet.
var ErrNoWebhookToken = errors.New("instance has no webhook token yet")

// WebhookConnection is the Arr notification connection subsyncd owns for one
// instance, with the Arr URL it was last verified against.
type WebhookConnection struct {
	ID        int64
	ArrURL    string
	CheckedAt time.Time
}

// NewWebhookToken returns 32 random bytes as 64 lowercase hex characters. A
// nil random reader means crypto/rand.
func NewWebhookToken(random io.Reader) (string, error) {
	if random == nil {
		random = rand.Reader
	}
	buf := make([]byte, 32)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", fmt.Errorf("generate webhook token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// EnsureWebhookTokens gives every named, already-ensured instance without a
// token a new one, in one transaction, and returns the token of every name.
func (r *Repository) EnsureWebhookTokens(ctx context.Context, names []string, random io.Reader) (map[string]string, error) {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin ensure webhook token: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	tokens := make(map[string]string, len(names))
	for _, name := range names {
		var token string
		if err := tx.QueryRowContext(ctx, `SELECT webhook_token FROM instances WHERE name=?`, name).Scan(&token); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrUnknownInstance
			}
			return nil, fmt.Errorf("read webhook token: %w", err)
		}
		if token == "" {
			if token, err = NewWebhookToken(random); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE instances SET webhook_token=? WHERE name=?`, token, name); err != nil {
				return nil, fmt.Errorf("store webhook token: %w", err)
			}
		}
		tokens[name] = token
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit ensure webhook token: %w", err)
	}
	return tokens, nil
}

// WebhookToken returns an instance's token.
func (r *Repository) WebhookToken(ctx context.Context, name string) (string, error) {
	var token string
	if err := r.store.db.QueryRowContext(ctx, `SELECT webhook_token FROM instances WHERE name=?`, name).Scan(&token); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrUnknownInstance
		}
		return "", fmt.Errorf("read webhook token: %w", err)
	}
	if token == "" {
		return "", ErrNoWebhookToken
	}
	return token, nil
}

// RotateWebhookToken replaces an instance's token with a new one.
func (r *Repository) RotateWebhookToken(ctx context.Context, name string, random io.Reader) error {
	token, err := NewWebhookToken(random)
	if err != nil {
		return err
	}
	result, err := r.store.db.ExecContext(ctx, `UPDATE instances SET webhook_token=? WHERE name=?`, token, name)
	if err != nil {
		return fmt.Errorf("rotate webhook token: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("rotate webhook token: %w", err)
	} else if n == 0 {
		return ErrUnknownInstance
	}
	return nil
}

// WebhookConnection returns the remembered connection, or the zero value when
// none was recorded.
func (r *Repository) WebhookConnection(ctx context.Context, name string) (WebhookConnection, error) {
	var connection WebhookConnection
	var checked int64
	err := r.store.db.QueryRowContext(ctx, `SELECT webhook_connection_id, webhook_connection_arr_url, webhook_checked_at_ns FROM instances WHERE name=?`, name).Scan(&connection.ID, &connection.ArrURL, &checked)
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookConnection{}, nil
	}
	if err != nil {
		return WebhookConnection{}, fmt.Errorf("read webhook connection: %w", err)
	}
	if checked > 0 {
		connection.CheckedAt = time.Unix(0, checked).UTC()
	}
	return connection, nil
}

// RecordWebhookConnection remembers the connection owned for an instance.
func (r *Repository) RecordWebhookConnection(ctx context.Context, name string, connection WebhookConnection) error {
	var checked int64
	if !connection.CheckedAt.IsZero() {
		checked = connection.CheckedAt.UnixNano()
	}
	if _, err := r.store.db.ExecContext(ctx, `UPDATE instances SET webhook_connection_id=?, webhook_connection_arr_url=?, webhook_checked_at_ns=? WHERE name=?`, connection.ID, connection.ArrURL, checked, name); err != nil {
		return fmt.Errorf("record webhook connection: %w", err)
	}
	return nil
}
