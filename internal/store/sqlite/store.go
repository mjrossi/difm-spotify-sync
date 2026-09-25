// Package sqlite is the persistence layer for the connector: the
// idempotency ledger, the review queue, and the sync-run log.
//
// The driver is modernc.org/sqlite (pure Go, no CGO) so the release
// binary stays statically linkable. Migrations are applied via goose
// from the embedded migrations-sqlite FS, so the binary needs nothing
// on disk at boot beyond the DB file itself.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pressly/goose/v3"
	sqlitedriver "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"

	sqlitegen "github.com/mjrossi/difm-spotify-sync/internal/store/sqlite/gen"
	migrations "github.com/mjrossi/difm-spotify-sync/migrations-sqlite"
)

// TimeFormat matches the strftime() default in the migrations so
// timestamps written by Go and by SQLite round-trip identically.
const TimeFormat = "2006-01-02T15:04:05.000Z"

// ErrCorrupt reports a database SQLite will not read: a truncated or
// half-written restore, a file that is not a database at all, or one
// whose pages no longer form a valid tree. Callers branch on it to
// suppress advice that does not apply — a corrupt file is not a
// permissions problem, and telling an operator to check both at once
// is worse than telling them neither.
var ErrCorrupt = errors.New("sqlite: database is unreadable")

// restoreHint is the one next step that applies to every ErrCorrupt.
// A path into docs/ is useless from the published image, which ships
// the binary and three scripts and nothing else — the same reason the
// engine's DI.fm line points at the README (engine.go) rather than a
// repo path.
const restoreHint = "restore from a backup — see the Restoring section of the deployment runbook, " +
	"https://github.com/mjrossi/difm-spotify-sync/blob/main/docs/deploy.md#restoring"

// Store wraps a *sql.DB with the sqlc-generated Queries.
type Store struct {
	db      *sql.DB
	q       *sqlitegen.Queries
	nowFunc func() time.Time
	log     *slog.Logger
	// inTx marks a Store handed to an InTx callback. SQLite allows one
	// writer, so a nested BeginTx blocks until the context expires —
	// and the production context is a signal context with no deadline,
	// which means it blocks forever. An immediate error beats a hang
	// that looks like the daemon wedging for no reason.
	inTx bool
}

// Open opens (or creates) the SQLite database at path. path may be a
// filesystem path or ":memory:". PRAGMAs are appended automatically;
// callers pass only the base path.
//
// Open does not apply migrations — call Migrate explicitly so callers
// decide when schema changes run.
//
// Open also refuses a database SQLite cannot read — truncated,
// non-SQLite, or failing its own integrity check — returning ErrCorrupt.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite.Open: empty path")
	}

	db, err := sql.Open("sqlite", dsnWithPragmas(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite.Open: %w", err)
	}
	// SQLite is single-writer; capping connections turns lock
	// contention into queueing rather than SQLITE_BUSY errors.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// sql.Open is lazy — Ping eagerly so a bad path fails at boot with a
	// clear error instead of leaking into the first query.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		// SQLITE_CORRUPT (11) and SQLITE_NOTADB (26) are what a bad
		// restore produces — a truncated or half-written docker cp, or a
		// file that is not a SQLite database at all. Both surface here
		// rather than at quick_check below, because SQLite validates the
		// page header when the connection opens, before any pragma runs.
		var derr *sqlitedriver.Error
		if errors.As(err, &derr) && (derr.Code() == 11 || derr.Code() == 26) {
			return nil, fmt.Errorf("sqlite.Open: %s: %w (%w); %s", path, ErrCorrupt, err, restoreHint)
		}
		return nil, fmt.Errorf("sqlite.Open: ping: %w", err)
	}

	// quick_check is integrity_check without the index pass: cheap enough
	// to run on every open, including the healthcheck's, and it is what
	// turns an in-place bit-flip (survives the ping above, since the
	// header is intact) into a message that names the file and the
	// runbook instead of a driver error from inside the first query.
	//
	// Its own budget rather than the ping's above: a slow ping must not
	// eat the time quick_check needs to walk a large database, and
	// separating them is what makes the 10s ceiling here safe rather than
	// generous — a 307 MB database checks in ~166ms, so 10s is headroom
	// for slow storage, not the expected case.
	qctx, qcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer qcancel()
	var verdict string
	if err := db.QueryRowContext(qctx, "PRAGMA quick_check").Scan(&verdict); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite.Open: %s: integrity check did not finish (%w); the database may be large or the storage slow — check DIFMSYNC_DB_PATH",
			path, err)
	}
	if verdict != "ok" {
		_ = db.Close()
		// verdict's first line is the constant banner "*** in database
		// main ***"; the real diagnosis is line two onward (e.g. "Tree 12
		// page 16: btreeInitPage() returns error code 11"). Drop the
		// banner rather than concatenate it — it carries no information
		// an operator can act on, only the detail does.
		_, detail, ok := strings.Cut(verdict, "\n")
		if !ok {
			detail = verdict
		}
		return nil, fmt.Errorf("sqlite.Open: %s: %w (%s); %s", path, ErrCorrupt, detail, restoreHint)
	}

	return &Store{
		db:      db,
		q:       sqlitegen.New(db),
		nowFunc: func() time.Time { return time.Now().UTC() },
		log:     slog.New(slog.DiscardHandler),
	}, nil
}

// dsnWithPragmas builds the modernc DSN. WAL for write throughput,
// foreign_keys because SQLite defaults it OFF and the schema relies on
// ON DELETE CASCADE, busy_timeout so transient locks wait rather than error.
func dsnWithPragmas(path string) string {
	pragmas := url.Values{}
	pragmas.Add("_pragma", "journal_mode(WAL)")
	pragmas.Add("_pragma", "foreign_keys(1)")
	pragmas.Add("_pragma", "busy_timeout(5000)")

	// A caller may already have appended their own query string, and
	// ":memory:" carries none. Append rather than assuming neither.
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + pragmas.Encode()
}

// migrateMu serializes Migrate. goose.SetBaseFS and goose.SetDialect are
// process-global, so two concurrent calls could interleave their setup.
// One boot-time call is the only real use, but the global state makes
// this cheap insurance rather than a theoretical concern.
var migrateMu sync.Mutex

// Migrate applies all embedded migrations. Safe to call on every boot.
func (s *Store) Migrate(ctx context.Context) error {
	migrateMu.Lock()
	defer migrateMu.Unlock()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("sqlite.Migrate: dialect: %w", err)
	}
	if err := goose.UpContext(ctx, s.db, "."); err != nil {
		return fmt.Errorf("sqlite.Migrate: %w", err)
	}
	return nil
}

// SchemaVersion reports the newest migration applied to this database, as
// goose's own version table records it.
//
// A direct query rather than goose's Provider.GetDBVersion, because that
// is not a read: it runs ensureVersionTable first, creating the table on a
// database that lacks one. The status endpoints call this unauthenticated,
// against whatever database they are handed — restored or hand-edited
// included — so it must not be able to write. The query is the one
// goose's own SQLite store runs for the same answer. Not in queries/:
// the table is goose's, outside the schema sqlc generates from.
//
// A database goose has never touched has no table, and this returns the
// error rather than reporting version 0.
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT MAX(version_id) FROM goose_db_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("sqlite.SchemaVersion: %w", err)
	}
	if !v.Valid {
		return 0, errors.New("sqlite.SchemaVersion: goose_db_version is empty")
	}
	return v.Int64, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// InTx runs fn inside a single transaction, committing if it returns nil
// and rolling back otherwise. The *Store handed to fn routes its queries
// through the transaction; everything else about it is shared.
//
// This exists so a sync pass can commit its ledger rows and its watermark
// as one unit. Written separately, a crash between them leaves a watermark
// ahead of the ledger, and the likes in that gap are never re-read.
//
// The pool is capped at one connection, so fn must not call InTx again or
// issue queries through the outer Store: either would deadlock waiting for
// a connection this transaction already holds.
func (s *Store) InTx(ctx context.Context, fn func(*Store) error) error {
	if s.inTx {
		return errors.New("sqlite.InTx: already in a transaction; " +
			"nesting would deadlock on SQLite's single writer")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite.InTx: begin: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this is safe
	// unconditionally and covers the panic path too.
	defer func() { _ = tx.Rollback() }()

	if err := fn(&Store{db: s.db, q: s.q.WithTx(tx), nowFunc: s.nowFunc, log: s.log, inTx: true}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite.InTx: commit: %w", err)
	}
	return nil
}

// SetLogger attaches a logger for the diagnostics the store can emit on
// its own — currently only a corrupted watermark. Unset, they are
// discarded rather than going to the default logger, so importing this
// package never hijacks a caller's logging setup.
//
// Call it before the store is in use; it is not safe against concurrent
// queries.
func (s *Store) SetLogger(l *slog.Logger) {
	if l != nil {
		s.log = l
	}
}

// SetClock overrides the time source for timestamps written by Go —
// sync_runs.started_at and finished_at. Columns defaulted by SQLite
// itself (created_at, resolved_at) come from strftime and are not
// affected, so a test cannot control every timestamp through this.
//
// Tests use it for deterministic run timing; production never calls it.
// Like SetLogger, it must be called before the store is in use.
func (s *Store) SetClock(f func() time.Time) { s.nowFunc = f }

func (s *Store) now() string { return s.nowFunc().UTC().Format(TimeFormat) }
