package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// This package runs three HTTP listeners — the status endpoints, the daemon's
// consent server, and the callback listener `difmsync auth` binds — and all
// three want the same thing: bind, serve in the background, report a serve
// failure on a channel, and drain on the way out.
//
// They were written three times, and the third had drifted on exactly the
// properties the other two document as load-bearing. `difmsync auth` set
// ReadHeaderTimeout alone, leaving ReadTimeout, WriteTimeout and IdleTimeout
// unset; it discarded Shutdown's error; and its goroutine sent nothing on a
// clean close, so a channel that meant "serving stopped" in two places meant
// "serving failed" in the third. None of that is visible at a call site —
// which is why it survived.
//
// Consolidating settled the first and the third outright. The second it turned
// from an omission into a choice: stop takes a logger, two callers pass one,
// and `difmsync auth` still passes nil — for the reason stated at that call.
// The change there is that the reason exists.

const (
	// Above the status handler's own timeout, so a slow database read
	// returns its own 503 rather than being cut off mid-body.
	//
	// The consent callback is the one handler that can plausibly approach
	// it, since it runs Spotify's token exchange inline. What the deadline
	// passing costs there is the browser's "Authorized" page and nothing
	// else: Complete runs under WithoutCancel, so the token is stored
	// regardless, and the terminal or the log reports the real outcome.
	serveWriteTimeout = 15 * time.Second
	serveIdleTimeout  = 60 * time.Second
	serveDrainTimeout = 5 * time.Second
)

// serving is a bound listener and the goroutine serving it.
type serving struct {
	// Addr is the resolved address, which is what the caller should log:
	// a request for ":0" or "0.0.0.0:3436" becomes a concrete port here.
	Addr net.Addr

	// Err receives exactly one value when serving stops: nil on a clean
	// shutdown, the failure otherwise. Buffered, so the goroutine never
	// leaks on a caller that stops selecting.
	Err <-chan error

	srv *http.Server
}

// serveHTTP binds addr and serves h on it. purpose names what the listener is
// for and appears in the bind error, which is the one an operator sees when a
// port is already taken.
//
// Request contexts derive from ctx, so canceling it unblocks anything in
// flight. Work that must outlive the request opts out explicitly with
// context.WithoutCancel — the consent exchange does, because a consent
// already given has to be stored even if the browser hangs up.
func serveHTTP(ctx context.Context, addr, purpose string, h http.Handler) (*serving, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s for %s: %w", addr, purpose, err)
	}

	// All four timeouts, not just the one the linter asks for. These
	// listeners are reachable by anything that can route to them, and a
	// connection opened and left idle otherwise holds a slot indefinitely.
	srv := &http.Server{
		Handler:           h,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      serveWriteTimeout,
		IdleTimeout:       serveIdleTimeout,
	}

	errc := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	return &serving{Addr: ln.Addr(), Err: errc, srv: srv}, nil
}

// stop drains the server and waits, bounded, for connections to finish.
// Safe to call more than once.
//
// The context is fresh rather than derived: on the shutdown path the caller's
// context is already canceled, and Shutdown given a canceled context returns
// immediately without draining anything — which is the whole thing it is
// being called to do.
func (s *serving) stop(log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), serveDrainTimeout)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil && log != nil {
		log.Warn("http server shutdown", "addr", s.Addr.String(), "err", err)
	}
}
