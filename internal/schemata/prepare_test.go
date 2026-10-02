/*
 * Copyright 2026 The Gremlins Authors
 *
 *    Licensed under the Apache License, Version 2.0 (the "License");
 *    you may not use this file except in compliance with the License.
 *    You may obtain a copy of the License at
 *
 *        http://www.apache.org/licenses/LICENSE-2.0
 *
 *    Unless required by applicable law or agreed to in writing, software
 *    distributed under the License is distributed on an "AS IS" BASIS,
 *    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *    See the License for the specific language governing permissions and
 *    limitations under the License.
 */

package schemata_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gremlins/gremlins/internal/engine"
	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/internal/log"
	"github.com/go-gremlins/gremlins/internal/mutator"
	"github.com/go-gremlins/gremlins/internal/schemata"
)

const prepareMod = "example.com/prepare"

// prepareModule is the prepare fixture as the engine sees it, run from the
// module root.
func prepareModule(t *testing.T) gomodule.GoModule {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "prepare"))
	if err != nil {
		t.Fatal(err)
	}

	return gomodule.GoModule{Name: prepareMod, Root: root, CallingDir: "."}
}

// streamMutants discovers the mutants of every non-test file of mod the way
// the engine's walk does -- file paths relative to the calling directory,
// every type the token table maps -- in walk order, all RUNNABLE.
func streamMutants(t *testing.T, mod gomodule.GoModule) []mutator.Mutator {
	t.Helper()
	var out []mutator.Mutator
	dir := filepath.Join(mod.Root, mod.CallingDir)
	err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".go" || strings.HasSuffix(p, "_test.go") {
			return err
		}
		src, err := os.ReadFile(filepath.Join(dir, p)) //nolint:gosec // G304: a fixture file
		if err != nil {
			return err
		}
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, p, src, parser.ParseComments)
		if err != nil {
			return err
		}
		pkg := mod.Name + "/" + path.Dir(p)
		ast.Inspect(file, func(n ast.Node) bool {
			node, ok := engine.NewTokenNode(n)
			if !ok {
				return true
			}
			types, ok := engine.MutantTypesFor(node)
			if !ok {
				return true
			}
			for _, mt := range types {
				m := engine.NewTokenMutant(pkg, set, file, node)
				m.SetType(mt)
				m.SetStatus(mutator.Runnable)
				out = append(out, m)
			}

			return true
		})

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// realNullRun runs bin in pkgDir the way a worker would, without
// GREMLINS_MUTANT, and fails with its output.
func realNullRun(bin, pkgDir string) error {
	cmd := exec.Command(bin, "-test.count=1")
	cmd.Dir = pkgDir
	cmd.Env = slicesWithout(os.Environ(), "GREMLINS_MUTANT=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}

	return nil
}

func slicesWithout(env []string, prefix string) []string {
	var out []string
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}

	return out
}

// nullRuns records the null runs Prepare makes and fails them on demand.
type nullRuns struct {
	mu    sync.Mutex
	calls map[string]int // by package directory
	dirs  map[string]string
	// builds is the build each binary's null run was handed.
	builds map[string]*schemata.Build
	fail   func(pkgDir string, call int) error
}

func (n *nullRuns) run(b *schemata.Build, bin, pkgDir string) error {
	n.mu.Lock()
	if n.calls == nil {
		n.calls = map[string]int{}
		n.dirs = map[string]string{}
		n.builds = map[string]*schemata.Build{}
	}
	n.calls[pkgDir]++
	call := n.calls[pkgDir]
	n.dirs[bin] = pkgDir
	n.builds[bin] = b
	n.mu.Unlock()
	if n.fail != nil {
		if err := n.fail(pkgDir, call); err != nil {
			return err
		}
	}

	return realNullRun(bin, pkgDir)
}

func ownPackage(pkg string) []string { return []string{pkg} }

// checkAccounting asserts every input mutant is in exactly one of placed and
// netted, that placed ids are the 1-based stream positions, and that every
// netted mutant has a reason.
func checkAccounting(t *testing.T, in []mutator.Mutator, plan schemata.Plan) {
	t.Helper()
	index := map[mutator.Mutator]int{}
	for i, m := range in {
		index[m] = i + 1
	}
	seen := map[mutator.Mutator]bool{}
	for _, p := range plan.Placed {
		if seen[p.Mutator] {
			t.Errorf("%s at %s accounted twice", p.Mutator.Type(), p.Mutator.Position())
		}
		seen[p.Mutator] = true
		if p.ID != index[p.Mutator] {
			t.Errorf("%s at %s: id %d, want its stream position %d", p.Mutator.Type(), p.Mutator.Position(), p.ID, index[p.Mutator])
		}
	}
	for _, n := range plan.Netted {
		if seen[n.Mutator] {
			t.Errorf("%s at %s accounted twice", n.Mutator.Type(), n.Mutator.Position())
		}
		seen[n.Mutator] = true
		if n.Reason == "" {
			t.Errorf("%s at %s netted without a reason", n.Mutator.Type(), n.Mutator.Position())
		}
	}
	if len(plan.Placed)+len(plan.Netted) != len(in) {
		t.Errorf("placed %d + netted %d != %d runnable", len(plan.Placed), len(plan.Netted), len(in))
	}
	for _, m := range in {
		if !seen[m] {
			t.Errorf("%s at %s is neither placed nor netted", m.Type(), m.Position())
		}
	}
}

// planByPkg is a plan's mutant types, and netted reasons, by package.
type planByPkg struct {
	placed, netted, reasons map[string][]string
}

func byPkg(plan schemata.Plan) planByPkg {
	g := planByPkg{placed: map[string][]string{}, netted: map[string][]string{}, reasons: map[string][]string{}}
	for _, p := range plan.Placed {
		g.placed[p.Mutator.Pkg()] = append(g.placed[p.Mutator.Pkg()], p.Mutator.Type().String())
	}
	for _, n := range plan.Netted {
		g.netted[n.Mutator.Pkg()] = append(g.netted[n.Mutator.Pkg()], n.Mutator.Type().String())
		g.reasons[n.Mutator.Pkg()] = append(g.reasons[n.Mutator.Pkg()], n.Reason)
	}

	return g
}

// TestPrepare is not parallel: it captures the process-wide log.
func TestPrepare(t *testing.T) {
	mod := prepareModule(t)
	in := streamMutants(t, mod)
	workDir := t.TempDir()
	runs := &nullRuns{fail: func(pkgDir string, call int) error {
		switch filepath.Base(pkgDir) {
		case "a":
			if call == 1 {
				return errors.New("flaky once")
			}
		case "c":
			return errors.New("boom first line\nsecond line")
		}

		return nil
	}}

	var out, eOut bytes.Buffer
	log.Init(&out, &eOut)
	defer log.Reset()

	plan, err := schemata.Prepare(context.Background(), mod, workDir, "", in, ownPackage, 2*time.Minute, runs.run)
	if err != nil {
		t.Fatal(err)
	}
	checkAccounting(t, in, plan)

	g := byPkg(plan)
	placed, netted, reasons := g.placed, g.netted, g.reasons
	a, b, c := prepareMod+"/a", prepareMod+"/b", prepareMod+"/c"
	if got := strings.Join(placed[a], ","); got != "ARITHMETIC_BASE" {
		t.Errorf("placed in a = %s, want ARITHMETIC_BASE (retried null run passes)", got)
	}
	if got := strings.Join(netted[a], ","); got != "INVERT_ASSIGNMENTS,REMOVE_SELF_ASSIGNMENTS" {
		t.Errorf("netted in a = %s, want the unsupported += mutants", got)
	}
	for _, r := range reasons[a] {
		if !strings.Contains(r, schemata.ErrUnsupported.Error()) {
			t.Errorf("a's netted reason %q does not name the unsupported site", r)
		}
	}
	if got := strings.Join(placed[b], ","); got != "INVERT_NEGATIVES,ARITHMETIC_BASE" {
		t.Errorf("placed in b = %s, want both mutants of its '-' (its test reads the original source)", got)
	}
	if len(placed[c]) != 0 || len(netted[c]) != 2 {
		t.Errorf("c: placed %v netted %v, want both mutants netted", placed[c], netted[c])
	}
	for _, r := range reasons[c] {
		if r != "null-mutant run failed: boom first line" {
			t.Errorf("c's netted reason = %q", r)
		}
	}

	// One null run per built binary, in the original package directory; a
	// second only after a failure.
	for _, tc := range []struct {
		dir   string
		calls int
	}{{"a", 2}, {"b", 1}, {"c", 2}} {
		dir := filepath.Join(mod.Root, tc.dir)
		if runs.calls[dir] != tc.calls {
			t.Errorf("null runs in %s = %d, want %d", dir, runs.calls[dir], tc.calls)
		}
	}
	for _, p := range []string{a, b, c} {
		bin, ok := plan.Build.Binaries[p]
		if !ok {
			t.Fatalf("no binary for %s", p)
		}
		if !strings.HasPrefix(bin, workDir) || !strings.HasPrefix(plan.Build.Dir, workDir) {
			t.Errorf("build output %s / %s is outside workDir %s", bin, plan.Build.Dir, workDir)
		}
		// The null run gets the build, for the overlay its go commands need.
		got := runs.builds[bin]
		if got == nil || got.Dir != plan.Build.Dir || got.Binaries[p] != bin || !slices.Equal(got.Rewritten, plan.Build.Rewritten) {
			t.Errorf("null run of %s was handed build %+v, want %+v", p, got, plan.Build)
		}
	}
	// The fixture is not vacuous: b's binary fails in the schema copy, where
	// the helper file sits beside its source.
	if err := realNullRun(plan.Build.Binaries[b], filepath.Join(plan.Build.Dir, "b")); err == nil {
		t.Error("b's binary passes in the schema copy: the original-source check proves nothing")
	}

	if !strings.Contains(out.String(), "schemata: built 3 packages in ") {
		t.Errorf("info log lacks the build line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), fmt.Sprintf("schemata: %d mutants placed, %d netted", len(plan.Placed), len(plan.Netted))) {
		t.Errorf("info log lacks the counts:\n%s", out.String())
	}
	for _, n := range plan.Netted {
		line := fmt.Sprintf("schemata: %s at %s goes through the per-mutant path: %s\n", n.Mutator.Type(), n.Mutator.Position(), n.Reason)
		if !strings.Contains(eOut.String(), line) {
			t.Errorf("error log lacks %q", line)
		}
	}
}

func TestPrepareNets(t *testing.T) {
	t.Parallel()
	a, b := prepareMod+"/a", prepareMod+"/b"
	testCases := map[string]struct {
		testPkgs func(string) []string
		mutate   func([]mutator.Mutator) []mutator.Mutator
		// wantNetted maps a package to a substring every netted reason of
		// it contains; its placeable mutants must all be netted.
		wantNetted map[string]string
		wantPlaced []string
	}{
		"a mutant whose test package fails to build": {
			testPkgs: func(p string) []string {
				if p == a {
					return []string{p, prepareMod + "/nosuch"}
				}

				return []string{p}
			},
			wantNetted: map[string]string{a: "nosuch"},
			wantPlaced: []string{b},
		},
		"a mutant at no operator of the loaded source": {
			mutate: func(in []mutator.Mutator) []mutator.Mutator {
				for i, m := range in {
					if m.Pkg() == b {
						in[i] = movedMutant{Mutator: m, line: 999}
					}
				}

				return in
			},
			wantNetted: map[string]string{b: "no mutation site"},
			wantPlaced: []string{a},
		},
		"a mutant whose import path is not its directory's": {
			mutate: func(in []mutator.Mutator) []mutator.Mutator {
				for i, m := range in {
					if m.Pkg() == b {
						in[i] = movedMutant{Mutator: m, pkg: prepareMod + "/elsewhere"}
					}
				}

				return in
			},
			wantNetted: map[string]string{prepareMod + "/elsewhere": "elsewhere"},
			wantPlaced: []string{a},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mod := prepareModule(t)
			in := streamMutants(t, mod)
			var kept []mutator.Mutator
			for _, m := range in {
				if m.Pkg() != prepareMod+"/c" && m.Type() != mutator.InvertAssignments && m.Type() != mutator.RemoveSelfAssignments {
					kept = append(kept, m)
				}
			}
			if tc.mutate != nil {
				kept = tc.mutate(kept)
			}
			testPkgs := tc.testPkgs
			if testPkgs == nil {
				testPkgs = ownPackage
			}
			runs := &nullRuns{}
			plan, err := schemata.Prepare(context.Background(), mod, t.TempDir(), "", kept, testPkgs, 2*time.Minute, runs.run)
			if err != nil {
				t.Fatal(err)
			}
			checkAccounting(t, kept, plan)
			g := byPkg(plan)
			placed, netted, reasons := g.placed, g.netted, g.reasons
			for p, want := range tc.wantNetted {
				if len(placed[p]) != 0 || len(netted[p]) == 0 {
					t.Errorf("%s: placed %v netted %v, want all netted", p, placed[p], netted[p])
				}
				for _, r := range reasons[p] {
					if !strings.Contains(r, want) {
						t.Errorf("%s netted with %q, want it to contain %q", p, r, want)
					}
				}
			}
			for _, p := range tc.wantPlaced {
				if len(netted[p]) != 0 || len(placed[p]) == 0 {
					t.Errorf("%s: placed %v netted %v (%v), want all placed", p, placed[p], netted[p], reasons[p])
				}
			}
		})
	}
}

func TestPrepareWithNothingToPlace(t *testing.T) {
	t.Parallel()
	runs := &nullRuns{}
	plan, err := schemata.Prepare(context.Background(), prepareModule(t), t.TempDir(), "", nil, ownPackage, time.Minute, runs.run)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Placed) != 0 || len(plan.Netted) != 0 || len(plan.Build.Binaries) != 0 || len(runs.calls) != 0 {
		t.Errorf("empty input gave %+v and %d null runs", plan, len(runs.calls))
	}
}

func TestPrepareStopsWhenCancelled(t *testing.T) {
	t.Parallel()
	mod := prepareModule(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runs := &nullRuns{}
	if _, err := schemata.Prepare(ctx, mod, t.TempDir(), "", streamMutants(t, mod), ownPackage, time.Minute, runs.run); !errors.Is(err, context.Canceled) {
		t.Errorf("Prepare on a cancelled context: err = %v, want context.Canceled", err)
	}
}

// movedMutant reports a different line or package than the mutant it wraps.
type movedMutant struct {
	mutator.Mutator
	line int
	pkg  string
}

func (m movedMutant) Position() token.Position {
	p := m.Mutator.Position()
	if m.line != 0 {
		p.Line = m.line
	}

	return p
}

func (m movedMutant) Pkg() string {
	if m.pkg != "" {
		return m.pkg
	}

	return m.Mutator.Pkg()
}
