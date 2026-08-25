package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
// by a variable that is named in a comment.
//
// The value comparison needs only the two files that state defaults, so the
// first cut of this test read only those. That silently narrowed the old
// recipe, which scanned nine — and two of the three drifts named above lived
// in files the narrowed set no longer reached. configPaths below restores the
// coverage; the value checks keep the reason it was rewritten.
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

// configPaths is every committed place a DIFMSYNC_* name can be set, shipped
// or written down. README.md and the Dockerfile appear here as well as in the
// value checks below, because those two regexes read one table and one ENV
// block — a dead variable mentioned in README prose is invisible to both.
//
// Directories are listed rather than their files, so a new doc or workflow is
// covered the day it is added rather than the day someone remembers this list.
var configPaths = []string{
	".env.local.example",
	".github/workflows",
	"Dockerfile",
	"README.md",
	"compose.yaml",
	"docker",
	"docs",
	"justfile",
	"mise.toml",
}

// anyEnvName matches a DIFMSYNC_* name anywhere, prose included. That is the
// point of it: an operator who reads a variable in a sentence will try to set
// it, and a name no flag reads costs them the same afternoon whether it was
// written in a table or in a paragraph.
var anyEnvName = regexp.MustCompile(`DIFMSYNC_[A-Z_]+`)

// configRefs returns every DIFMSYNC_* name under configPaths, mapped to the
// files naming it so a failure says where to go.
func configRefs(t *testing.T) map[string][]string {
	t.Helper()
	repo := filepath.Join("..", "..")
	refs := map[string][]string{}

	for _, p := range configPaths {
		err := filepath.WalkDir(filepath.Join(repo, p), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(repo, path)
			if err != nil {
				rel = path
			}
			for _, name := range anyEnvName.FindAllString(string(b), -1) {
				if !slices.Contains(refs[name], rel) {
					refs[name] = append(refs[name], rel)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", p, err)
		}
	}

	// A path renamed out from under this list would empty the scan and pass
	// every assertion built on it, which is the failure this whole file
	// exists to make impossible.
	if len(refs) == 0 {
		t.Fatalf("no DIFMSYNC_* names found under %v; the paths have moved and this check is now vacuous", configPaths)
	}
	return refs
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

	// The orphan direction, over every file rather than the two the value
	// checks read. A variable set in compose.yaml, exported by a workflow or
	// written into docs/ that no flag reads is config an operator can set and
	// then wonder why nothing happened — DIFMSYNC_NETWORK sat in mise.toml
	// like that, and the docs carried a DIFM_* prefix nothing had ever read.
	t.Run("nothing set or documented anywhere is unread by the binary", func(t *testing.T) {
		for name, files := range configRefs(t) {
			if _, ok := flags[name]; !ok {
				t.Errorf("%s appears in %s but no flag reads it — dead config",
					name, strings.Join(files, ", "))
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

// miseEnv matches one assignment in mise.toml's [env] table.
var miseEnv = regexp.MustCompile(`(?m)^(DIFMSYNC_[A-Z_]+)\s*=\s*"([^"]*)"`)

// shellRef matches $DIFMSYNC_X or ${DIFMSYNC_X...} in the justfile, i.e. a
// non-Go tool reading the value out of the environment.
var shellRef = regexp.MustCompile(`\$\{?(DIFMSYNC_[A-Z_]+)`)

// mise.toml's [env] is the checkout's environment, and it drifts the same way
// the Dockerfile's ENV block did: by accumulating restatements of values the
// binary already defaults to. DIFMSYNC_INTERVAL and DIFMSYNC_NETWORK were both
// sitting there set to exactly the flag default, which buys nothing and gives
// the value a second home that nothing keeps in sync.
//
// A variable earns its place by doing one of two jobs:
//
//  1. differing from the flag default — a development preference, which is
//     the point of a checkout having its own environment at all; or
//  2. being read from the environment by a non-Go tool. goose and sqlite3
//     cannot see a Go flag default, so `just db` and friends can only open
//     the same database difmsync uses if the path is exported.
//
// Job 2 is detected by looking for the variable in the justfile rather than
// from a hardcoded list, so this stays true as recipes come and go.
func TestMiseEnvEarnsItsPlace(t *testing.T) {
	flags := envFlags(t)
	mise := repoFile(t, "mise.toml")
	justfile := repoFile(t, "justfile")

	// [env] only — [tools] and the trailing _.file line are not assignments
	// this should judge.
	body := mise
	if i := strings.Index(body, "\n[env]"); i >= 0 {
		body = body[i:]
	}

	shellRead := map[string]bool{}
	for _, m := range shellRef.FindAllStringSubmatch(justfile, -1) {
		shellRead[m[1]] = true
	}

	for _, m := range miseEnv.FindAllStringSubmatch(body, -1) {
		name, value := m[1], m[2]

		refs, ok := flags[name]
		if !ok {
			t.Errorf("mise.toml sets %s but no flag reads it — dead config", name)
			continue
		}
		if !sameValue(value, refs[0].value) {
			continue // job 1: a genuine development preference
		}
		if shellRead[name] {
			continue // job 2: a non-Go tool reads it from the environment
		}
		t.Errorf("mise.toml sets %s=%q, which is already the flag default, "+
			"and no justfile recipe reads it from the environment. "+
			"Drop it — a second copy of a default is a second thing to keep in sync.",
			name, value)
	}
}

// exampleAssignment matches an uncommented assignment in .env.local.example.
// The leading anchor is what makes it a credential list: the file also carries
// DIFMSYNC_SPOTIFY_REDIRECT_URL, commented out, because that one is a setting
// an operator may need rather than a secret.
var exampleAssignment = regexp.MustCompile(`(?m)^(DIFMSYNC_[A-Z_]+)=`)

// credentials derives the secret set from .env.local.example rather than from
// a list here. A list is the shape of check this repo keeps getting caught by:
// it passes for the sixth credential exactly as it did for the first five, and
// nothing about adding one prompts anybody to come back and extend it.
// .env.local.example is the file that already has to name every secret, since
// it is what an operator copies.
func credentials(t *testing.T) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, m := range exampleAssignment.FindAllStringSubmatch(repoFile(t, ".env.local.example"), -1) {
		found[m[1]] = true
	}
	if len(found) == 0 {
		t.Fatal(".env.local.example names no credentials; this check derives its list from that file " +
			"and has just become vacuous")
	}
	return found
}

// Credentials belong in .env.local, which mise.toml loads and compose.yaml
// also reads as its only env_file. A secret parked in [env] instead would be
// committed, and would reach the host tooling but not the container — the
// half-working state that is hardest to diagnose.
func TestMiseEnvHoldsNoCredentials(t *testing.T) {
	mise := repoFile(t, "mise.toml")
	secret := credentials(t)

	body := mise
	if i := strings.Index(body, "\n[env]"); i >= 0 {
		body = body[i:]
	}
	for _, m := range miseEnv.FindAllStringSubmatch(body, -1) {
		if secret[m[1]] {
			t.Errorf("%s is a credential (it is in .env.local.example) and must live in .env.local, "+
				"not in committed mise.toml", m[1])
		}
	}

	// Tolerant of spacing and quote style, so reformatting mise.toml cannot
	// fail this for a reason that has nothing to do with credentials.
	if !miseLoadsEnvLocal.MatchString(mise) {
		t.Error("mise.toml no longer loads .env.local; the host tooling and the container " +
			"would stop sharing one copy of the credentials")
	}
}

// miseLoadsEnvLocal matches the _.file line that makes mise.toml and
// compose.yaml share one copy of the secrets.
var miseLoadsEnvLocal = regexp.MustCompile(`_\.file\s*=\s*["']\.env\.local["']`)

// TestGoVersionPinsAgree checks that the three places pinning the Go
// toolchain state the same patch version.
//
// The pins are mise.toml (what `just check` and the govulncheck workflow
// run), go.mod (the language version), and the Dockerfile's builder
// image (what compiles the *published* binary). Only the first is what
// CI scans.
//
// So the drift is silent in exactly the direction that matters: let the
// Dockerfile fall behind and govulncheck reports "No vulnerabilities
// found" against mise's toolchain while every released image is built on
// the stale one and ships the CVEs. Nothing fails, and the scan that
// exists to catch this is the thing reporting green. That is how
// 1.26.5 shipped with five called stdlib vulnerabilities.
//
// A comment in the Dockerfile already said the pins were kept in sync.
// It was accurate when written, which is the problem a comment cannot
// solve and a test can.
func TestGoVersionPinsAgree(t *testing.T) {
	pins := []struct {
		file string
		re   *regexp.Regexp
	}{
		{"mise.toml", regexp.MustCompile(`(?m)^go\s*=\s*"([0-9]+\.[0-9]+\.[0-9]+)"`)},
		{"go.mod", regexp.MustCompile(`(?m)^go\s+([0-9]+\.[0-9]+\.[0-9]+)`)},
		{"Dockerfile", regexp.MustCompile(`golang:([0-9]+\.[0-9]+\.[0-9]+)-`)},
	}

	got := make(map[string]string, len(pins))
	for _, p := range pins {
		m := p.re.FindStringSubmatch(repoFile(t, p.file))
		if m == nil {
			t.Fatalf("%s: no Go version pin found; if the pin moved, update this test rather than deleting it", p.file)
		}
		got[p.file] = m[1]
	}

	want := got["mise.toml"]
	for file, v := range got {
		if v != want {
			t.Errorf("%s pins Go %s, mise.toml pins %s; all three must agree so the published image is built on the toolchain CI scanned", file, v, want)
		}
	}
}
