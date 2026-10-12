// Package control implements the local Unix-socket protocol the CLI uses to
// hand commands to a running daemon.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
)

const (
	// ProtocolVersion is the wire version both sides must speak.
	ProtocolVersion = 1
	// SocketName is the socket file name inside the data directory.
	SocketName = "subsyncd.sock"
	// MaxRequestBytes bounds one request.
	MaxRequestBytes = 64 << 10
)

// Routed reports whether the running daemon executes command itself.
func Routed(command string) bool {
	return command == "retry" || command == "search" || command == "reject" || command == "scan"
}

// Error codes carried in Error.Code.
const (
	CodeUsage            = "usage"
	CodeUnknownInstance  = "unknown_instance"
	CodeUnknownProvider  = "unknown_provider"
	CodeUnknownLanguage  = "unknown_language"
	CodeMediaNotFound    = "media_not_found"
	CodeAmbiguousMedia   = "ambiguous_media"
	CodeNotManaged       = "not_managed"
	CodeBusy             = "busy"
	CodeDaemonRequired   = "daemon_required"
	CodeDaemonStopping   = "daemon_stopping"
	CodeProtocolMismatch = "protocol_mismatch"
	CodeArrUnreachable   = "arr_unreachable"
	CodeInternal         = "internal"
)

// MediaMatch describes one media file a command could refer to.
type MediaMatch struct {
	Instance   string `json:"instance"`
	Kind       string `json:"kind"`
	FileID     int64  `json:"file_id"`
	Title      string `json:"title"`
	Year       int    `json:"year,omitempty"`
	Season     int    `json:"season,omitempty"`
	Episode    int    `json:"episode,omitempty"`
	EpisodeEnd int    `json:"episode_end,omitempty"`
}

// Error is a failure reported by the daemon (or synthesized by the client).
type Error struct {
	Code       string       `json:"code"`
	Message    string       `json:"message"`
	Candidates []MediaMatch `json:"candidates,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Request is the single message a client sends per connection.
type Request struct {
	Version int             `json:"version"`
	Command string          `json:"command"`
	Args    json.RawMessage `json:"args,omitempty"`
}

// Response is the single message the daemon returns.
type Response struct {
	Version int             `json:"version"`
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Handler executes one command. An error that is not a *Error is reported as
// CodeInternal.
type Handler interface {
	HandleControl(ctx context.Context, command string, args json.RawMessage) (any, error)
}

// SocketPath returns the control socket location for a data directory.
func SocketPath(dataDir string) string {
	return filepath.Join(dataDir, SocketName)
}

// pathFree describes a failed operation without the file path that
// *net.OpError and *fs.PathError embed in their messages. It keeps the errno
// or context cause so errors.Is still works.
func pathFree(op string, err error) error {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cause) {
			return fmt.Errorf("%s: %w", op, cause)
		}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("%s: %w", op, errno)
	}
	return errors.New(op)
}
