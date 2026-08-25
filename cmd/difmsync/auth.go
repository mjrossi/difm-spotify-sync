package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"

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
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name: "manual",
				Usage: "do not listen for the callback; print the consent URL and read the " +
					"URL the browser was redirected to from stdin (works with no inbound " +
					"networking at all, and with an https redirect URL)",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if err := requireFlags(c, "member-id", "playlist-id",
				"spotify-client-id", "spotify-client-secret"); err != nil {
				return err
			}

			redirect := c.String("spotify-redirect-url")

			// Derived only for the listening flow. --manual never binds
			// anything, so it must not inherit callbackTarget's
			// http-scheme restriction: an operator whose registered
			// redirect URI is https has no listener to terminate TLS for,
			// and does not need one.
			var target callback
			if !c.Bool("manual") {
				var err error
				target, err = callbackTarget(redirect, c.String("auth-bind"))
				if err != nil {
					return err
				}
			}

			return withStore(ctx, c, func(store *sqlite.Store) error {
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

				if c.Bool("manual") {
					return runManualConsent(ctx, flow, redirect, os.Stdin, os.Stdout)
				}

				results := make(chan error, 1)

				mux := http.NewServeMux()
				mux.HandleFunc(target.Path, func(w http.ResponseWriter, r *http.Request) {
					// Completed in the handler rather than by handing the code
					// back to the select below, so the state check, the
					// exchange and the token write stay in one place shared
					// with the daemon's consent server. Splitting them across
					// the two entry points is how the two drift.
					results <- completeConsentRequest(w, r, flow,
						"Authorized. You can close this tab and return to the terminal.", "")
				})

				srv, err := serveHTTP(ctx, target.Addr, "the OAuth callback", mux)
				if err != nil {
					return err
				}
				// nil logger: this is a foreground command, and a drain warning
				// belongs in the operator's terminal only if it changes what they
				// do — it does not, since the outcome is already reported below.
				defer srv.stop(nil)

				printConsentURL(os.Stdout, flow)
				fmt.Printf("Waiting for the callback on %s (listening on %s, timeout %s)...\n",
					redirect, target.Addr, authCallbackTimeout)

				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(authCallbackTimeout):
					return errAuthTimeout
				case err := <-srv.Err:
					// Kept separate from results: a listener that died and a
					// consent that was refused need different fixes, and
					// reporting one as the other sends the operator to the
					// wrong half of the problem.
					if err != nil {
						return fmt.Errorf("callback listener: %w", err)
					}
					return errors.New("callback listener stopped before the callback arrived")
				case err := <-results:
					if err != nil {
						return err
					}
					fmt.Println(consentStoredMsg)
					return nil
				}
			})
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
// Written once because both consent flows print them and the two had already
// been edited independently. The listening flow writes to stdout and the
// manual flow to an injected writer, which is the only reason these take one.
const consentStoredMsg = "Refresh token stored. `difmsync sync` can now run unattended."

// errNoCallbackPasted is returned when the manual flow gets an empty paste,
// from either the scanner or the parser.
var errNoCallbackPasted = errors.New("no callback URL was pasted")

// printConsentURL writes the authorize URL an operator has to open. Indented
// and surrounded by blank lines so it survives being copied out of a terminal
// that has wrapped it.
func printConsentURL(out io.Writer, flow *consentFlow) {
	fmt.Fprintln(out, "Open this URL to authorize Spotify access:")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "   ", flow.AuthURL())
	fmt.Fprintln(out)
}

func callbackTarget(redirect, bind string) (callback, error) {
	u, err := parseRedirect(redirect)
	if err != nil {
		return callback{}, err
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

// runManualConsent is the consent flow for a deployment nothing can reach
// from a browser.
//
// The listening flow needs the browser and the daemon to meet on a port:
// on the Docker host that is a loopback literal, and from anywhere else
// it needs something terminating TLS, because Spotify refuses a
// non-loopback redirect URI over plain HTTP. On a NAS, a VPS, or behind
// CGNAT that is a real wall. Here the browser never has to reach anything
// — the operator carries the callback back by hand — so the redirect URL
// only has to be registered, not reachable, and its scheme does not
// matter.
//
// It goes through flow.Complete like the other two entry points, so the
// state check, the denied-consent branch and the empty-refresh-token
// guard are the same code. That is the whole reason consentFlow exists:
// three transports, one set of security obligations.
func runManualConsent(ctx context.Context, flow *consentFlow, redirect string,
	in io.Reader, out io.Writer,
) error {
	fmt.Fprintln(out, "Open this URL to authorize Spotify access:")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "   ", flow.AuthURL())
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Approve it, then paste the URL your browser ended up at.\n"+
		"It starts with %s and the page will almost certainly fail to load —\n"+
		"that is expected, nothing is listening there. Only the address bar matters.\n\n",
		redirect)
	fmt.Fprint(out, "Redirected URL: ")

	// A Scanner rather than a Reader so a pasted line that arrives without
	// a trailing newline (which is what a terminal paste with no Enter
	// looks like on some shells) is still read.
	sc := bufio.NewScanner(in)
	// Authorization codes are long and the URL carries the state as well;
	// the 64KB default has been known to be too small for a pasted URL
	// with extra parameters, and truncation here would surface as a
	// state mismatch, which sends the operator looking in the wrong place.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return fmt.Errorf("read the pasted callback URL: %w", err)
		}
		return errNoCallbackPasted
	}

	q, err := manualCallbackQuery(sc.Text())
	if err != nil {
		return err
	}
	if err := flow.Complete(ctx, q); err != nil {
		return err
	}
	fmt.Fprintln(out, "\n"+consentStoredMsg)
	return nil
}

// manualCallbackQuery pulls the OAuth parameters out of whatever the
// operator pasted.
//
// Deliberately lenient about the shape of the paste and strict about its
// content. People copy address bars inconsistently — the whole URL, just
// the query string, sometimes wrapped in quotes by a shell — and none of
// those differences change what the flow needs. What it will not do is
// accept a bare authorization code: the state parameter traveling beside
// it is the CSRF guard, and synthesizing one to make the paste work would
// remove that guard on this path only, which is exactly the divergence
// consentFlow exists to prevent.
func manualCallbackQuery(pasted string) (url.Values, error) {
	in := strings.TrimSpace(pasted)
	in = strings.Trim(in, `"'`)
	if in == "" {
		return nil, errNoCallbackPasted
	}

	raw := in
	if i := strings.Index(in, "?"); i >= 0 {
		raw = in[i+1:]
	}
	// Trailing fragment, if the browser or the paste carried one.
	if i := strings.Index(raw, "#"); i >= 0 {
		raw = raw[:i]
	}

	// Neither error echoes the paste back. It is an authorization code
	// most of the time, and in the bare-code case below it is one by
	// definition — single-use and useless without the client secret, but
	// this is the one path where credential material arrives as an
	// argument, and errors get pasted into issues. The messages say what
	// to do instead, which is what the operator needs anyway.
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, fmt.Errorf("could not parse the pasted text as a callback URL: %w", err)
	}
	if q.Get("code") == "" && q.Get("error") == "" {
		return nil, errors.New("no code parameter in what you pasted — paste the whole URL " +
			"the browser was redirected to, query string included (it looks like " +
			"…/callback?code=…&state=…). The code on its own is not enough: the state " +
			"beside it is what proves the callback belongs to this flow")
	}
	return q, nil
}
