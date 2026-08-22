package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

// errAuthTimeout is returned when nobody completes the browser step.
var errAuthTimeout = errors.New("timed out waiting for Spotify consent callback")

// authCallbackTimeout bounds how long the local listener waits for the
// browser round-trip before giving up and freeing the port.
const authCallbackTimeout = 5 * time.Minute

// authCommand runs the one-time Spotify consent flow. It stands up a
// local HTTP listener on the redirect URL's port, prints the consent URL,
// and exchanges the returned code for a refresh token — after which the
// daemon runs unattended indefinitely.
func authCommand() *cli.Command {
	return &cli.Command{
		Name:  "auth",
		Usage: "one-time Spotify OAuth consent; stores the refresh token",
		Action: func(ctx context.Context, c *cli.Command) error {
			if err := requireFlags(c, "member-id", "playlist-id",
				"spotify-client-id", "spotify-client-secret"); err != nil {
				return err
			}

			redirect := c.String("spotify-redirect-url")
			target, err := callbackTarget(redirect, c.String("auth-bind"))
			if err != nil {
				return err
			}

			store, err := openStore(ctx, c)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			account, err := store.EnsureAccount(ctx, c.String("account"),
				c.String("member-id"), c.String("playlist-id"))
			if err != nil {
				return err
			}

			auth := spotify.NewAuthenticator(c.String("spotify-client-id"),
				c.String("spotify-client-secret"), redirect)

			flow, err := newConsentFlow(auth, store, account.ID)
			if err != nil {
				return err
			}

			results := make(chan error, 1)

			mux := http.NewServeMux()
			mux.HandleFunc(target.Path, func(w http.ResponseWriter, r *http.Request) {
				// Completed here rather than by handing the code back to
				// the select below, so the state check, the exchange and
				// the token write stay in one place shared with the
				// daemon's consent server. Splitting them across the two
				// entry points is how the two drift.
				//
				// WithoutCancel because a browser that closes the tab the
				// instant the callback lands would otherwise cancel the
				// token exchange mid-flight, losing a consent the operator
				// has already given.
				if err := flow.Complete(context.WithoutCancel(r.Context()), r.URL.Query()); err != nil {
					http.Error(w, "Consent failed: "+err.Error(), http.StatusBadRequest)
					results <- err
					return
				}
				fmt.Fprintln(w, "Authorized. You can close this tab and return to the terminal.")
				results <- nil
			})

			srv := &http.Server{
				Addr:              target.Addr,
				Handler:           mux,
				ReadHeaderTimeout: 10 * time.Second,
			}
			var lc net.ListenConfig
			ln, err := lc.Listen(ctx, "tcp", target.Addr)
			if err != nil {
				return fmt.Errorf("listen on %s for the OAuth callback: %w", target.Addr, err)
			}
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					results <- err
				}
			}()
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdownCtx)
			}()

			fmt.Println("Open this URL to authorize Spotify access:")
			fmt.Println()
			fmt.Println("   ", flow.AuthURL())
			fmt.Println()
			fmt.Printf("Waiting for the callback on %s (listening on %s, timeout %s)...\n",
				redirect, target.Addr, authCallbackTimeout)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(authCallbackTimeout):
				return errAuthTimeout
			case err := <-results:
				if err != nil {
					return err
				}
				fmt.Println("Refresh token stored. `difmsync sync` can now run unattended.")
				return nil
			}
		},
	}
}

// callback describes where the local listener must sit for the configured
// redirect URL to actually reach it.
type callback struct {
	Addr string // host:port to bind
	Path string // request path to serve
}

// callbackTarget derives the listener from the redirect URL, so the two
// cannot drift apart. All three components matter: binding the right port
// on the wrong path 404s and hangs until the timeout, which looks exactly
// like Spotify never calling back.
//
// bind overrides only the host. It exists for containers: the redirect
// URL has to stay a loopback address (Spotify's dashboard requires one,
// and the browser is on the host), but a container's published port
// forwards to its eth0 address, not its loopback — so a listener bound
// inside the container to 127.0.0.1 is unreachable from the host no
// matter how the port is published. Setting DIFMSYNC_AUTH_BIND=0.0.0.0
// is what makes `docker compose run --service-ports connector auth`
// work.
func callbackTarget(redirect, bind string) (callback, error) {
	u, err := url.Parse(redirect)
	if err != nil {
		return callback{}, fmt.Errorf("parse redirect url %q: %w", redirect, err)
	}
	if u.Host == "" {
		return callback{}, fmt.Errorf("redirect url %q has no host", redirect)
	}
	// Checked before anything is derived from it: the listener serves
	// plain HTTP and cannot terminate TLS, so an https redirect would
	// bind (as root, on 443) and then fail every callback.
	if u.Scheme != "http" {
		return callback{}, fmt.Errorf("redirect url %q must be http: the callback listener "+
			"serves plain HTTP on loopback and cannot terminate TLS", redirect)
	}
	// A bind override is a host, never a host:port — the port is the
	// redirect URL's, since that is what Spotify will call back to.
	// Without this, DIFMSYNC_AUTH_BIND=0.0.0.0:9000 yields a listen error
	// that says nothing about the actual mistake.
	if strings.Contains(bind, ":") && net.ParseIP(bind) == nil {
		return callback{}, fmt.Errorf("auth bind %q must be a host or IP without a port "+
			"(the port comes from the redirect url)", bind)
	}

	port := u.Port()
	if port == "" {
		port = "8888"
	}

	// Serve whatever path the redirect URL actually carries. Defaulting a
	// pathless URL to "/callback" reintroduced the bug this derivation
	// exists to prevent: Spotify would call back to "/", the mux would
	// 404, and the command would hang for the full timeout looking
	// exactly like Spotify never responding. "/" is a catch-all pattern,
	// which is harmless on a single-purpose listener that shuts down
	// after one callback.
	path := u.Path
	if path == "" {
		path = "/"
	}

	host := u.Hostname()
	if bind != "" {
		host = bind
	}
	return callback{Addr: net.JoinHostPort(host, port), Path: path}, nil
}
