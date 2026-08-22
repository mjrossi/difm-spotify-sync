package status

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
)

// handlerTimeout bounds a single request's database work.
//
// The store is capped at one connection (SetMaxOpenConns(1)), so a read
// issued while a sync pass holds its transaction does not fail — it
// queues. Without a deadline a long pass turns into a hung request and,
// for whatever is polling /healthz, an indistinguishable-from-down
// timeout. Failing fast with a 503 is the honest answer.
const handlerTimeout = 5 * time.Second

// Handler serves the read-only operator endpoints:
//
//	GET /healthz      200 "ok" | 503 with the reason
//	GET /status.json  the Report as JSON
//
// Read-only is a design constraint, not an accident of scope. These
// endpoints are exposed to the LAN without authentication, which is only
// defensible because they cannot change anything and carry no secrets.
// Approving a queued match stays a CLI action — it writes to Spotify, and
// the ordering it depends on lives in `difmsync review --approve`.
func Handler(store *sqlite.Store, label string, maxAge time.Duration, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), handlerTimeout)
		defer cancel()

		// DefaultRunLimit rather than 1: this handler renders no runs, but
		// asking for fewer used to narrow the health scan too, which made
		// /healthz disagree with `status --check` for the duration of every
		// pass. Build no longer ties the two together; passing the same
		// value as every other caller keeps that obvious.
		rep, err := Build(ctx, store, label, maxAge, DefaultRunLimit)
		if err != nil {
			writeText(w, http.StatusServiceUnavailable, err.Error(), log)
			return
		}
		if !rep.Healthy {
			writeText(w, http.StatusServiceUnavailable, rep.Reason, log)
			return
		}
		writeText(w, http.StatusOK, "ok", log)
	})

	mux.HandleFunc("GET /status.json", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), handlerTimeout)
		defer cancel()

		rep, err := Build(ctx, store, label, maxAge, DefaultRunLimit)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			encode(w, map[string]string{"error": err.Error()}, log)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// 200 even when unhealthy: this endpoint reports state, and
		// "healthy": false *is* the state. /healthz is the one that
		// answers with a status code.
		encode(w, rep, log)
	})

	return mux
}

func writeText(w http.ResponseWriter, code int, body string, log *slog.Logger) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	if _, err := w.Write([]byte(body + "\n")); err != nil {
		log.Debug("writing response body failed", "err", err)
	}
}

func encode(w http.ResponseWriter, v any, log *slog.Logger) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The header is already written, so this cannot become an error
		// response. Logging it is all that is left.
		log.Warn("encoding response failed", "err", err)
	}
}
