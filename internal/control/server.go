package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"subsyncd/internal/observability"
)

const (
	readTimeout  = 30 * time.Second
	writeTimeout = 10 * time.Second
	// defaultDrainTimeout bounds how long shutdown waits for handlers after
	// canceling them, like the manual-search drain.
	defaultDrainTimeout = 30 * time.Second
)

// Server serves the control protocol on a Unix socket. The caller must hold
// the data-directory lock, so a pre-existing socket file is stale.
type Server struct {
	Path    string
	Handler Handler
	// Redact turns an unexpected handler error into a client-safe message.
	Redact func(error) string
	Events *observability.Emitter
	// DrainTimeout bounds the shutdown wait for in-flight handlers after
	// their context is canceled (30 seconds when zero). A handler still
	// running then, stuck in uninterruptible I/O, is left behind with its
	// connection closed.
	DrainTimeout time.Duration
}

// Serve listens until ctx is canceled, then drains in-flight connections and
// removes the socket. It returns nil on a clean shutdown.
func (s *Server) Serve(ctx context.Context) error {
	if info, err := os.Lstat(s.Path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("control socket path exists and is not a socket")
		}
		if err := os.Remove(s.Path); err != nil {
			return pathFree("remove stale control socket", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return pathFree("inspect control socket", err)
	}
	listener, err := net.Listen("unix", s.Path)
	if err != nil {
		return pathFree("listen on control socket", err)
	}
	if err := os.Chmod(s.Path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(s.Path)
		return pathFree("restrict control socket", err)
	}

	handlerCtx, cancelHandlers := context.WithCancel(ctx)
	defer cancelHandlers()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns = map[net.Conn]struct{}{}
	)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		// Unblock connections that are still waiting for a request.
		mu.Lock()
		for c := range conns {
			_ = c.SetReadDeadline(time.Now())
		}
		mu.Unlock()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			cancelHandlers()
			wg.Wait()
			_ = listener.Close()
			_ = os.Remove(s.Path)
			return pathFree("accept control connection", err)
		}
		mu.Lock()
		conns[conn] = struct{}{}
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				mu.Lock()
				delete(conns, conn)
				mu.Unlock()
				_ = conn.Close()
			}()
			s.serveConn(handlerCtx, conn)
		}()
	}

	cancelHandlers()
	s.drain(&wg, &mu, conns)
	_ = os.Remove(s.Path)
	return nil
}

// drain waits for the canceled handlers, at most DrainTimeout. Handlers still
// running then have their connections closed and are left behind.
func (s *Server) drain(wg *sync.WaitGroup, mu *sync.Mutex, conns map[net.Conn]struct{}) {
	timeout := s.DrainTimeout
	if timeout <= 0 {
		timeout = defaultDrainTimeout
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		mu.Lock()
		for c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	limited := &countingReader{r: io.LimitReader(conn, MaxRequestBytes+1)}
	var req Request
	if err := json.NewDecoder(limited).Decode(&req); err != nil || limited.n > MaxRequestBytes {
		switch {
		case ctx.Err() != nil:
			s.write(conn, failure(&Error{Code: CodeDaemonStopping, Message: "subsyncd is shutting down"}))
		case limited.n > MaxRequestBytes:
			s.write(conn, failure(&Error{Code: CodeUsage, Message: "request is too large"}))
		default:
			s.write(conn, failure(&Error{Code: CodeUsage, Message: "request is not valid JSON"}))
		}
		return
	}
	if req.Version != ProtocolVersion {
		s.write(conn, failure(&Error{Code: CodeProtocolMismatch, Message: mismatchMessage(req.Version, ProtocolVersion, true)}))
		return
	}
	value, err := s.Handler.HandleControl(ctx, req.Command, req.Args)
	if err != nil {
		var ce *Error
		switch {
		case errors.As(err, &ce):
			s.write(conn, failure(ce))
		case ctx.Err() != nil:
			s.write(conn, failure(&Error{Code: CodeDaemonStopping, Message: "subsyncd is shutting down"}))
		default:
			message := "internal error"
			if s.Redact != nil {
				message = s.Redact(err)
			}
			s.write(conn, failure(&Error{Code: CodeInternal, Message: message}))
		}
		return
	}
	var raw json.RawMessage
	if value != nil {
		raw, err = json.Marshal(value)
		if err != nil {
			s.write(conn, failure(&Error{Code: CodeInternal, Message: "internal error"}))
			return
		}
	}
	s.write(conn, Response{Version: ProtocolVersion, OK: true, Result: raw})
}

func failure(e *Error) Response {
	return Response{Version: ProtocolVersion, Error: e}
}

func (s *Server) write(conn net.Conn, resp Response) {
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_ = json.NewEncoder(conn).Encode(resp)
}

func mismatchMessage(other, mine int, fromServer bool) string {
	if fromServer {
		return fmt.Sprintf("subsyncd CLI speaks control protocol %d but the daemon speaks %d", other, mine)
	}
	return fmt.Sprintf("subsyncd CLI speaks control protocol %d but the daemon speaks %d", mine, other)
}
