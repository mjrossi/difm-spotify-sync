package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// The three listeners in this package were written separately, and the one
// `difmsync auth` used had only ReadHeaderTimeout — the other three timeouts
// silently absent. Nothing failed, no test noticed, and the divergence was
// invisible from the call site. Pin all four here so a future edit that drops
// one fails in CI rather than in a deployment.
func TestServeHTTPSetsEveryTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := serveHTTP(ctx, "127.0.0.1:0", "a test", http.NewServeMux())
	if err != nil {
		t.Fatalf("serveHTTP: %v", err)
	}
	defer srv.stop(nil)

	for _, tc := range []struct {
		name string
		got  time.Duration
	}{
		{"ReadHeaderTimeout", srv.srv.ReadHeaderTimeout},
		{"ReadTimeout", srv.srv.ReadTimeout},
		{"WriteTimeout", srv.srv.WriteTimeout},
		{"IdleTimeout", srv.srv.IdleTimeout},
	} {
		if tc.got <= 0 {
			t.Errorf("%s is unset; an idle connection holds a slot indefinitely", tc.name)
		}
	}

	// A slow handler must be able to return its own 503 rather than being
	// cut off mid-body, so the write budget has to exceed the handler's own.
	// internal/status.handlerTimeout is 5s and unexported, so it is restated
	// rather than imported; if it grows past this, this assertion is the
	// thing that should notice.
	const statusHandlerTimeout = 5 * time.Second
	if srv.srv.WriteTimeout <= statusHandlerTimeout {
		t.Errorf("WriteTimeout %v does not exceed the status handler's %v",
			srv.srv.WriteTimeout, statusHandlerTimeout)
	}
	if srv.srv.BaseContext == nil {
		t.Error("BaseContext is nil; request contexts will not observe shutdown")
	}
}

// stop is deferred on paths that may also have returned early, so it has to
// tolerate being called after the server is already down. It is also what
// makes the consent listener die when awaitConsent returns.
func TestServingStopIsIdempotent(t *testing.T) {
	srv, err := serveHTTP(context.Background(), "127.0.0.1:0", "a test", http.NewServeMux())
	if err != nil {
		t.Fatalf("serveHTTP: %v", err)
	}
	srv.stop(nil)
	srv.stop(nil)

	select {
	case err := <-srv.Err:
		if err != nil {
			t.Errorf("clean shutdown reported %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve goroutine did not report after shutdown")
	}
}
