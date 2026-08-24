package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
)

// The DIFMSYNC_* surface is stated in four places: the flag definitions in
// this package, the Dockerfile's ENV block, the README's configuration table,
// and whatever an operator sets. Nothing but a check keeps the first three
// agreeing, and four of fifteen variables had already drifted once — a dead
// DIFMSYNC_NETWORK, a DIFM_* prefix in the docs, a redirect URL missing from
// the example.
//
// This replaces a shell recipe that compared variable *names* only, and so
// could not see the drift that actually matters: an image default of 15m
// against a README that says 5m sends an operator debugging a sync interval
// that was never in effect. It also greps the whole README rather than the
// table, so a variable mentioned once in prose counted as documented.
//
// Reading the real command tree rather than the source text is what makes the
// value comparison possible at all. It also means the check cannot be fooled
// by a variable that is named in a comment, and needs no maintenance when a
// config file is added or removed.
//
// Note this reads flag *definitions*, never the process environment, so it
// needs no clearEnv and passes identically on a machine with a populated
// .env.local.

// readmeRow matches one row of the configuration table and nothing else.
// Anchoring on the leading pipe plus a backticked DIFMSYNC_ name is the
// specific thing the old grep could not do: prose mentioning a variable does
// not start a table row, so it no longer counts as documentation.
var readmeRow = regexp.MustCompile(`(?m)^\|\s*` + "`" + `(DIFMSYNC_[A-Z_]+)` + "`" + `\s*\|([^|]*)\|([^|]*)\|`)

// dockerEnv matches one NAME=value pair inside the Dockerfile's ENV block.
var dockerEnv = regexp.MustCompile(`(?m)^\s*(?:ENV\s+)?(DIFMSYNC_[A-Z_]+)=(\S+)`)

// firstBackticked pulls the primary value out of a table cell, ignoring any
// trailing parenthetical. "`/config/difmsync.db` (CLI: `./tmp/difmsync.db`)"
// documents two defaults; the first is the one the image ships, which is what
// this table column claims to state.
var firstBackticked = regexp.MustCompile("`([^`]*)`")

type flagRef struct {
	command string // "" for a root flag
	name    string
	value   string
}

// envFlags walks the real command tree and returns every flag carrying a
// DIFMSYNC_* env source, keyed by variable name. A variable read by more than
// one flag (--max-age is on both sync and status) yields more than one entry,
// which is what lets the duplicate-default check below exist.
func envFlags(t *testing.T) map[string][]flagRef {
	t.Helper()
	found := map[string][]flagRef{}

	collect := func(command string, flags []cli.Flag) {
		for _, f := range flags {
			doc, ok := f.(cli.DocGenerationFlag)
			if !ok {
				t.Fatalf("flag %q does not implement cli.DocGenerationFlag", f.Names()[0])
			}
			for _, env := range f.(interface{ GetEnvVars() []string }).GetEnvVars() {
				if !strings.HasPrefix(env, "DIFMSYNC_") {
					continue
				}
				found[env] = append(found[env], flagRef{
					command: command,
					name:    f.Names()[0],
					// GetValue quotes strings but not numbers or durations.
					value: strings.Trim(doc.GetValue(), `"`),
				})
			}
		}
	}

	app := newApp()
	collect("", app.Flags)
	for _, c := range app.Commands {
		collect(c.Name, c.Flags)
	}
	return found
}

func repoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// sameValue compares two rendered defaults for equality of meaning rather
// than of text. "15m" and "15m0s" are the same interval; "0.60" and "0.6" are
// the same threshold. Without this the check would force the README to spell
// its values the way Go's flag package renders them, which is the wrong way
// round — the table is written for an operator.
func sameValue(a, b string) bool {
	if a == b {
		return true
	}
	if da, err := time.ParseDuration(a); err == nil {
		if db, err := time.ParseDuration(b); err == nil {
			return da == db
		}
	}
	if fa, err := strconv.ParseFloat(a, 64); err == nil {
		if fb, err := strconv.ParseFloat(b, 64); err == nil {
			return fa == fb
		}
	}
	return false
}

func TestConfigSurfaceIsDocumentedAndConsistent(t *testing.T) {
	flags := envFlags(t)
	readme := repoFile(t, "README.md")
	dockerfile := repoFile(t, "Dockerfile")

	// The image's ENV block. A variable absent here takes the binary's own
	// default in the container, which is the rule the Dockerfile states.
	image := map[string]string{}
	for _, m := range dockerEnv.FindAllStringSubmatch(dockerfile, -1) {
		image[m[1]] = strings.TrimSuffix(m[2], `\`)
	}

	// The README table, which is the documented surface.
	documented := map[string]string{}
	for _, m := range readmeRow.FindAllStringSubmatch(readme, -1) {
		name, defaultCell := m[1], strings.TrimSpace(m[3])
		if v := firstBackticked.FindStringSubmatch(defaultCell); v != nil {
			documented[name] = v[1]
		} else {
			// "— (required)" and friends: no default at all.
			documented[name] = ""
		}
	}

	t.Run("every variable the binary reads has a README table row", func(t *testing.T) {
		for name, refs := range flags {
			if _, ok := documented[name]; !ok {
				t.Errorf("%s is read by --%s but has no row in the README configuration table;\n"+
					"a knob nobody can discover is a knob nobody can use", name, refs[0].name)
			}
		}
	})

	t.Run("nothing is documented or shipped that the binary never reads", func(t *testing.T) {
		for name := range documented {
			if _, ok := flags[name]; !ok {
				t.Errorf("%s has a README table row but no flag reads it — dead config", name)
			}
		}
		for name := range image {
			if _, ok := flags[name]; !ok {
				t.Errorf("%s is set in the Dockerfile ENV block but no flag reads it — dead config", name)
			}
		}
	})

	t.Run("the documented default is the one the image actually ships", func(t *testing.T) {
		for name, want := range documented {
			refs, ok := flags[name]
			if !ok {
				continue // reported above
			}
			// The effective container default: the image's override if it
			// sets one, otherwise the binary's own.
			got, source := refs[0].value, "the flag default"
			if v, ok := image[name]; ok {
				got, source = v, "the Dockerfile ENV block"
			}
			if !sameValue(want, got) {
				t.Errorf("%s: README table says %q, %s says %q", name, want, source, got)
			}
		}
	})

	// --max-age is defined on both `sync` and `status`, with the same env
	// source. Two literals that must agree and nothing making them: the whole
	// failure mode this file exists for, in miniature.
	t.Run("a variable read by several flags has one default", func(t *testing.T) {
		for name, refs := range flags {
			for _, r := range refs[1:] {
				if !sameValue(refs[0].value, r.value) {
					t.Errorf("%s: %s --%s defaults to %q but %s --%s defaults to %q",
						name, refs[0].command, refs[0].name, refs[0].value,
						r.command, r.name, r.value)
				}
			}
		}
	})
}
