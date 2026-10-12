package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
)

// ErrNoDaemon reports that nothing is listening on the control socket.
var ErrNoDaemon = errors.New("no subsyncd daemon is running")

// Client is one connection to the daemon; it carries a single call.
type Client struct {
	conn net.Conn
}

// Dial connects to the daemon's control socket in dataDir.
func Dial(ctx context.Context, dataDir string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", SocketPath(dataDir))
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrNoDaemon
		}
		return nil, fmt.Errorf("connect to control socket: %w", err)
	}
	return &Client{conn: conn}, nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Call sends one command and decodes the daemon's result into result (which
// may be nil). A failure reported by the daemon is returned as *Error.
func (c *Client) Call(ctx context.Context, command string, args, result any) error {
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return fmt.Errorf("encode control arguments: %w", err)
		}
		raw = b
	}
	stop := context.AfterFunc(ctx, func() { _ = c.conn.Close() })
	defer stop()

	transport := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Code: CodeDaemonStopping, Message: "subsyncd closed the connection without answering"}
	}
	if err := json.NewEncoder(c.conn).Encode(Request{Version: ProtocolVersion, Command: command, Args: raw}); err != nil {
		return transport()
	}
	var resp Response
	if err := json.NewDecoder(c.conn).Decode(&resp); err != nil {
		return transport()
	}
	if resp.Version != ProtocolVersion {
		return &Error{Code: CodeProtocolMismatch, Message: mismatchMessage(resp.Version, ProtocolVersion, false)}
	}
	if !resp.OK {
		if resp.Error == nil {
			return &Error{Code: CodeInternal, Message: "daemon reported a failure without details"}
		}
		return resp.Error
	}
	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("decode control result: %w", err)
		}
	}
	return nil
}
