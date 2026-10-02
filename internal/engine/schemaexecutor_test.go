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

package engine_test

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"

	"github.com/go-gremlins/gremlins/internal/configuration"
	"github.com/go-gremlins/gremlins/internal/coverage"
	"github.com/go-gremlins/gremlins/internal/engine"
	"github.com/go-gremlins/gremlins/internal/engine/workdir"
	"github.com/go-gremlins/gremlins/internal/engine/workerpool"
	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/internal/mutator"
	"github.com/go-gremlins/gremlins/internal/schemata"
)

const (
	calcPkg = "schemaexec/calc"
	usePkg  = "schemaexec/use"
)

// schemaMutators are the mutators the schemata prototype rewrites.
var schemaMutators = []mutator.Type{
	mutator.ArithmeticBase, mutator.ConditionalsBoundary, mutator.ConditionalsNegation,
	mutator.IncrementDecrement, mutator.InvertNegatives,
}

// fixtureMutant is one mutant of the fixture: its schema ID and position.
type fixtureMutant struct {
	id  int
	pos token.Position
}

// schemaFixture is the testdata/schemaexec module built into schema test
// binaries, with its mutants keyed "Func/MUTATOR".
type schemaFixture struct {
	mod   gomodule.GoModule
	build schemata.Build
	muts  map[string]fixtureMutant
}

// buildSchemaFixture discovers the mutants of the calc package, rewrites it
// and builds the test binaries of calc and use from the schema copy.
func buildSchemaFixture(t *testing.T) schemaFixture {
	t.Helper()
	modRoot, err := filepath.Abs("testdata/schemaexec")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedSyntax,
		Dir: modRoot,
	}
	pkgs, err := packages.Load(cfg, "./calc")
	if err != nil || len(pkgs) != 1 || len(pkgs[0].Errors) > 0 {
		t.Fatalf("load calc: %v %v", err, pkgs)
	}
	pkg := pkgs[0]

	muts := map[string]fixtureMutant{}
	var sites []schemata.Site
	for _, f := range pkg.Syntax {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				tn, ok := engine.NewTokenNode(n)
				if !ok {
					return true
				}
				mts, _ := engine.MutantTypesFor(tn)
				site := schemata.Site{Node: n, Tok: tn.Tok()}
				for _, mt := range mts {
					if !slices.Contains(schemaMutators, mt) {
						continue
					}
					id := len(muts) + 1
					muts[fn.Name.Name+"/"+mt.String()] = fixtureMutant{id: id, pos: pkg.Fset.Position(tn.TokPos)}
					site.Muts = append(site.Muts, schemata.Mutant{ID: id, Type: mt})
				}
				if len(site.Muts) > 0 {
					sites = append(sites, site)
				}

				return true
			})
		}
	}

	files, _, dropped := schemata.RewritePackage(pkg, sites, "")
	if len(dropped) > 0 {
		t.Fatalf("sites dropped: %v", dropped)
	}
	b, errs := schemata.BuildAll(context.Background(), modRoot, t.TempDir(), "",
		map[string]map[string][]byte{calcPkg: files}, []string{calcPkg, usePkg}, 5*time.Minute)
	for p, err := range errs {
		if err != nil {
			t.Fatalf("build %s: %v", p, err)
		}
	}

	return schemaFixture{
		mod:   gomodule.GoModule{Name: "schemaexec", Root: modRoot, CallingDir: "."},
		build: b,
		muts:  muts,
	}
}

// newSchemaDealer returns a dealer over fresh worker copies of the fixture
// whose test timeout is one second, with extra configuration set.
func newSchemaDealer(t *testing.T, fx schemaFixture, set map[string]any, opts ...engine.ExecutorDealerOption) (*engine.MutantExecutorDealer, workdir.Dealer) {
	t.Helper()
	wdd := workdir.NewCachedDealer(t.TempDir(), fx.mod.Root)
	t.Cleanup(wdd.Clean)
	cfg := map[string]any{configuration.UnleashTimeoutMaxKey: "1s"}
	maps.Copy(cfg, set)
	viperSet(cfg)
	d := engine.NewExecutorDealer(fx.mod, wdd, 0, opts...)
	viperReset()

	return d, wdd
}

// runSchemaMutant executes the fixture mutant key on worker w with build b.
func runSchemaMutant(t *testing.T, d *engine.MutantExecutorDealer, fx schemaFixture, key string, b *schemata.Build, w *workerpool.Worker) *mutantStub {
	t.Helper()
	fm, ok := fx.muts[key]
	if !ok {
		t.Fatalf("no fixture mutant %s in %v", key, slices.Sorted(maps.Keys(fx.muts)))
	}
	stub := &mutantStub{pkg: calcPkg, position: fm.pos, status: mutator.Runnable}
	outCh := make(chan mutator.Mutator, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	d.NewSchemaExecutor(stub, fm.id, b, outCh, &wg).Start(w)
	wg.Wait()
	if got := <-outCh; got != stub {
		t.Fatalf("executor sent %v, want the mutant it was given", got)
	}

	return stub
}

// cmdRecorder records every command an executor creates.
type cmdRecorder struct {
	mu   sync.Mutex
	cmds []*exec.Cmd
}

func (r *cmdRecorder) exec(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: test code running the executor's own command
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmd)

	return cmd
}

func (r *cmdRecorder) all() []*exec.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.cmds)
}

func envValue(env []string, key string) (string, bool) {
	var val string
	var found bool
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			val, found = v, true // the last one wins, as in os/exec
		}
	}

	return val, found
}

// TestSchemaExecutor runs fixture mutants against prebuilt schema binaries.
func TestSchemaExecutor(t *testing.T) {
	t.Parallel()
	fx := buildSchemaFixture(t)
	t.Run("verdicts", func(t *testing.T) { testSchemaVerdicts(t, fx) })
	t.Run("invocation", func(t *testing.T) { testSchemaInvocation(t, fx) })
	t.Run("missing_binary_falls_back", func(t *testing.T) { testSchemaMissingBinaryFallsBack(t, fx) })
	t.Run("run_cancelled", func(t *testing.T) { testSchemaRunCancelled(t, fx) })
	t.Run("cancelled_mid_run", func(t *testing.T) { testSchemaCancelledMidRun(t, fx) })
	t.Run("concurrent_workers_isolated", func(t *testing.T) { testSchemaConcurrentWorkersIsolated(t, fx) })
}

// testSchemaVerdicts holds each verdict the fixture can reach to the one its
// mutant must get.
func testSchemaVerdicts(t *testing.T, fx schemaFixture) {
	t.Parallel()
	testCases := map[string]struct {
		key  string
		set  map[string]any
		opts []engine.ExecutorDealerOption
		want mutator.Status
	}{
		"killed":        {key: "Add/ARITHMETIC_BASE", want: mutator.Killed},
		"lived":         {key: "Scale/ARITHMETIC_BASE", want: mutator.Lived},
		"not_covered":   {key: "Unused/ARITHMETIC_BASE", want: mutator.NotCovered},
		"run_timed_out": {key: "Pause/CONDITIONALS_BOUNDARY", want: mutator.RunTimedOut},
		"increment":     {key: "Inc/INCREMENT_DECREMENT", want: mutator.Killed},
		"negative":      {key: "Neg/INVERT_NEGATIVES", want: mutator.Killed},
		"killed_in_dependent": {
			key:  "Scale/ARITHMETIC_BASE",
			set:  map[string]any{configuration.UnleashCrossPackageKey: true},
			opts: []engine.ExecutorDealerOption{engine.WithDependents(dependentsStub{calcPkg: {usePkg}})},
			want: mutator.Killed,
		},
		"selected_tests_do_not_reach": {
			key: "Scale/ARITHMETIC_BASE",
			opts: []engine.ExecutorDealerOption{engine.WithTestSelection(selectorStub{
				mapped: map[string]bool{calcPkg: true},
				tests:  []coverage.TestID{{Pkg: calcPkg, Name: "TestAdd"}},
			})},
			want: mutator.NotCovered,
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, _ := newSchemaDealer(t, fx, tc.set, tc.opts...)
			got := runSchemaMutant(t, d, fx, tc.key, &fx.build, workerpool.NewWorker(1, "w"))
			if got.Status() != tc.want {
				t.Errorf("status = %s, want %s", got.Status(), tc.want)
			}
			if got.applyCalled {
				t.Error("the schema executor applied the mutation to the worker copy")
			}
		})
	}
}

// testSchemaInvocation checks the command the executor runs: flags, directory,
// environment, overlay, a stale reach file cleared, the worker copy untouched.
func testSchemaInvocation(t *testing.T, fx schemaFixture) {
	t.Parallel()
	rec := &cmdRecorder{}
	d, wdd := newSchemaDealer(t, fx, map[string]any{configuration.UnleashTestCPUKey: 2},
		engine.WithExecContext(rec.exec),
		engine.WithTestSelection(selectorStub{
			mapped: map[string]bool{calcPkg: true},
			tests:  []coverage.TestID{{Pkg: calcPkg, Name: "TestAdd"}, {Pkg: calcPkg, Name: "TestScale"}},
		}))
	w := workerpool.NewWorker(3, "w")
	root, err := wdd.Get("w-3")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "calc", "calc.go")) //nolint:gosec // G304: test code reading its worker copy
	if err != nil {
		t.Fatal(err)
	}
	fm := fx.muts["Unused/ARITHMETIC_BASE"]
	reach := filepath.Join(root, ".reached-"+strconv.Itoa(fm.id))
	if err := os.WriteFile(reach, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got := runSchemaMutant(t, d, fx, "Unused/ARITHMETIC_BASE", &fx.build, w)
	if got.Status() != mutator.NotCovered {
		t.Errorf("status with a stale reach file = %s, want NOT COVERED", got.Status())
	}
	runSchemaMutant(t, d, fx, "Add/ARITHMETIC_BASE", &fx.build, w)

	cmds := rec.all()
	if len(cmds) != 2 {
		t.Fatalf("%d commands, want 2", len(cmds))
	}
	cmd := cmds[0]
	wantArgs := []string{
		fx.build.Binaries[calcPkg], "-test.count=1", "-test.timeout", "1s", "-test.failfast",
		"-test.run", "^(TestAdd|TestScale)$", "-test.cpu", "2",
	}
	if !slices.Equal(cmd.Args, wantArgs) {
		t.Errorf("args = %q, want %q", cmd.Args, wantArgs)
	}
	if want := filepath.Join(root, "calc"); cmd.Dir != want {
		t.Errorf("dir = %s, want %s", cmd.Dir, want)
	}
	if v, _ := envValue(cmd.Env, "GREMLINS_MUTANT"); v != strconv.Itoa(fm.id) {
		t.Errorf("GREMLINS_MUTANT = %q, want %d", v, fm.id)
	}
	if v, _ := envValue(cmd.Env, "GREMLINS_REACHED"); v != reach {
		t.Errorf("GREMLINS_REACHED = %q, want %s", v, reach)
	}
	overlay := overlayPath(t, cmd.Env)
	if other := overlayPath(t, cmds[1].Env); other != overlay {
		t.Errorf("second mutant on the worker used overlay %s, want the worker's one %s", other, overlay)
	}
	raw, err := os.ReadFile(overlay) //nolint:gosec // G304: test code reading the executor's overlay
	if err != nil {
		t.Fatal(err)
	}
	var o struct{ Replace map[string]string }
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, rel := range fx.build.Rewritten {
		want[filepath.Join(root, rel)] = filepath.Join(fx.build.Dir, rel)
	}
	if len(want) == 0 || !maps.Equal(o.Replace, want) {
		t.Errorf("overlay = %v, want %v", o.Replace, want)
	}
	after, err := os.ReadFile(filepath.Join(root, "calc", "calc.go")) //nolint:gosec // G304: test code reading its worker copy
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the worker copy's source changed")
	}
}

// testSchemaMissingBinaryFallsBack checks that a selected package without a
// binary sends the mutant through the legacy executor.
func testSchemaMissingBinaryFallsBack(t *testing.T, fx schemaFixture) {
	t.Parallel()
	rec := &cmdRecorder{}
	d, _ := newSchemaDealer(t, fx, nil, engine.WithExecContext(rec.exec))
	partial := fx.build
	partial.Binaries = map[string]string{usePkg: fx.build.Binaries[usePkg]}

	got := runSchemaMutant(t, d, fx, "Add/ARITHMETIC_BASE", &partial, workerpool.NewWorker(1, "w"))

	if !got.applyCalled || !got.rollbackCalled {
		t.Error("the mutant did not go through the legacy executor")
	}
	cmds := rec.all()
	if len(cmds) != 1 || len(cmds[0].Args) < 2 || cmds[0].Args[0] != "go" || cmds[0].Args[1] != "test" {
		t.Errorf("commands %v, want one go test", cmds)
	}
	// The stub's Apply changes nothing, so go test runs the original suite.
	if got.Status() != mutator.Lived {
		t.Errorf("status = %s, want the legacy verdict LIVED", got.Status())
	}
}

// testSchemaRunCancelled checks that a run cancelled before the mutant starts
// takes the shutdown status and starts nothing.
func testSchemaRunCancelled(t *testing.T, fx schemaFixture) {
	t.Parallel()
	rec := &cmdRecorder{}
	d, _ := newSchemaDealer(t, fx, nil, engine.WithExecContext(rec.exec))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.SetRunCtx(ctx)

	got := runSchemaMutant(t, d, fx, "Add/ARITHMETIC_BASE", &fx.build, workerpool.NewWorker(1, "w"))

	if got.Status() != mutator.NotCovered {
		t.Errorf("status = %s, want the default shutdown status NOT COVERED", got.Status())
	}
	if n := len(rec.all()); n != 0 {
		t.Errorf("%d commands started after the run was cancelled", n)
	}
}

// A run cancelled after the site was reached takes the shutdown status,
// not the LIVED the reach would otherwise make it.
func testSchemaCancelledMidRun(t *testing.T, fx schemaFixture) {
	t.Parallel()
	d, _ := newSchemaDealer(t, fx, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.SetRunCtx(ctx)
	time.AfterFunc(300*time.Millisecond, cancel)

	got := runSchemaMutant(t, d, fx, "Pause/CONDITIONALS_BOUNDARY", &fx.build, workerpool.NewWorker(1, "w"))

	if got.Status() != mutator.NotCovered {
		t.Errorf("status = %s, want the default shutdown status NOT COVERED", got.Status())
	}
}

// testSchemaConcurrentWorkersIsolated runs eight mutants on four workers and
// expects the statuses a serial run gives.
func testSchemaConcurrentWorkersIsolated(t *testing.T, fx schemaFixture) {
	t.Parallel()
	keys := []string{
		"Add/ARITHMETIC_BASE", "Scale/ARITHMETIC_BASE", "Unused/ARITHMETIC_BASE", "Unused/INVERT_NEGATIVES",
		"Pause/CONDITIONALS_BOUNDARY", "Pause/CONDITIONALS_NEGATION", "Inc/INCREMENT_DECREMENT", "Neg/INVERT_NEGATIVES",
	}
	want := map[string]mutator.Status{
		"Add/ARITHMETIC_BASE": mutator.Killed, "Scale/ARITHMETIC_BASE": mutator.Lived,
		"Unused/ARITHMETIC_BASE": mutator.NotCovered, "Unused/INVERT_NEGATIVES": mutator.NotCovered,
		"Pause/CONDITIONALS_BOUNDARY": mutator.RunTimedOut, "Pause/CONDITIONALS_NEGATION": mutator.RunTimedOut,
		"Inc/INCREMENT_DECREMENT": mutator.Killed, "Neg/INVERT_NEGATIVES": mutator.Killed,
	}

	serialDealer, _ := newSchemaDealer(t, fx, nil)
	serial := map[string]mutator.Status{}
	w := workerpool.NewWorker(1, "serial")
	for _, k := range keys {
		serial[k] = runSchemaMutant(t, serialDealer, fx, k, &fx.build, w).Status()
	}
	if !maps.Equal(serial, want) {
		t.Fatalf("serial statuses %v, want %v", serial, want)
	}

	d, _ := newSchemaDealer(t, fx, nil)
	viperSet(map[string]any{configuration.UnleashWorkersKey: 4})
	pool := workerpool.Initialize("schema")
	viperReset()
	outCh := make(chan mutator.Mutator, len(keys))
	var wg sync.WaitGroup
	stubs := map[mutator.Mutator]string{}
	pool.Start()
	for _, k := range keys {
		fm := fx.muts[k]
		stub := &mutantStub{pkg: calcPkg, position: fm.pos, status: mutator.Runnable}
		stubs[stub] = k
		wg.Add(1)
		pool.AppendExecutor(d.NewSchemaExecutor(stub, fm.id, &fx.build, outCh, &wg))
	}
	wg.Wait()
	pool.Stop()
	close(outCh)
	concurrent := map[string]mutator.Status{}
	for m := range outCh {
		concurrent[stubs[m]] = m.Status()
	}
	if !maps.Equal(concurrent, serial) {
		t.Errorf("concurrent statuses %v, want the serial ones %v", concurrent, serial)
	}
}

// overlayPath returns the -overlay file named in env's GOFLAGS.
func overlayPath(t *testing.T, env []string) string {
	t.Helper()
	flags, _ := envValue(env, "GOFLAGS")
	for _, f := range strings.Fields(flags) {
		if p, ok := strings.CutPrefix(f, "-overlay="); ok {
			return p
		}
	}
	t.Fatalf("no -overlay in GOFLAGS %q", flags)

	return ""
}
