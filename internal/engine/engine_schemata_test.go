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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/tools/cover"

	"github.com/go-gremlins/gremlins/internal/configuration"
	"github.com/go-gremlins/gremlins/internal/coverage"
	"github.com/go-gremlins/gremlins/internal/engine"
	"github.com/go-gremlins/gremlins/internal/engine/workdir"
	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/internal/log"
	"github.com/go-gremlins/gremlins/internal/mutator"
	"github.com/go-gremlins/gremlins/internal/report"
	"github.com/go-gremlins/gremlins/internal/schemata"
)

// TestSchemataParity runs the engine over testdata/parity twice, without and
// with --schemata, and expects every mutant to get the same status both ways,
// with every runnable mutant judged against the schema binaries.
//
// It is not parallel: it captures the global log to read the report.
func TestSchemataParity(t *testing.T) {
	modRoot, err := filepath.Abs("testdata/parity")
	if err != nil {
		t.Fatal(err)
	}
	mod := gomodule.GoModule{Name: "parity", Root: modRoot, CallingDir: "."}
	prof := parityProfile(t, mod)

	legacy, legacyRes, legacyCalls := runParity(t, mod, prof, false)
	if legacyCalls != 0 {
		t.Errorf("the run without --schemata called Prepare %d times", legacyCalls)
	}
	if legacyRes.Schemata != nil {
		t.Errorf("the run without --schemata reports a schemata summary %+v", *legacyRes.Schemata)
	}

	// The fixture is only evidence if the statuses it compares are varied.
	seen := map[mutator.Status]int{}
	for _, s := range legacy {
		seen[s]++
	}
	for _, s := range []mutator.Status{mutator.Killed, mutator.Lived, mutator.NotCovered, mutator.RunTimedOut} {
		if seen[s] == 0 {
			t.Errorf("the fixture produced no %s mutant without --schemata: %v", s, legacy)
		}
	}
	runnable := len(legacy) - seen[mutator.NotCovered] - seen[mutator.Skipped]
	checkSharedLine(t, modRoot, legacy)

	keep := filepath.Join(t.TempDir(), "kept")
	t.Setenv("GREMLINS_SCHEMATA_KEEP", keep)
	withSchemata, res, calls := runParity(t, mod, prof, true)
	checkKept(t, keep, runnable)
	if calls != 1 {
		t.Errorf("the run with --schemata called Prepare %d times, want 1", calls)
	}
	if !maps.Equal(withSchemata, legacy) {
		for _, k := range slices.Sorted(maps.Keys(legacy)) {
			if withSchemata[k] != legacy[k] {
				t.Errorf("%s: %s with --schemata, %s without", k, withSchemata[k], legacy[k])
			}
		}
		if len(withSchemata) != len(legacy) {
			t.Errorf("%d mutants with --schemata, %d without", len(withSchemata), len(legacy))
		}
	}
	if res.Schemata == nil {
		t.Fatal("the run with --schemata has no schemata summary")
	}
	if want := (report.SchemataSummary{Placed: runnable, PerMutant: 0}); *res.Schemata != want {
		t.Errorf("schemata summary = %+v, want %+v", *res.Schemata, want)
	}

	out := &bytes.Buffer{}
	log.Init(out, &bytes.Buffer{})
	defer log.Reset()
	_ = report.Do(res)
	if want := fmt.Sprintf("Schemata: placed %d, per-mutant path 0\n", runnable); !strings.Contains(out.String(), want) {
		t.Errorf("report does not contain %q:\n%s", want, out.String())
	}
}

// TestSchemataSaysWhyItWasNotUsed runs --schemata where no schema build can
// be made, in a dry run and in integration mode, and requires the summary
// and the report line to say why placed is 0, not just print it.
//
// It is not parallel: it captures the global log to read the report.
func TestSchemataSaysWhyItWasNotUsed(t *testing.T) {
	modRoot, err := filepath.Abs("testdata/parity")
	if err != nil {
		t.Fatal(err)
	}
	mod := gomodule.GoModule{Name: "parity", Root: modRoot, CallingDir: "."}
	prof := parityProfile(t, mod)
	testCases := map[string]struct {
		extra   map[string]any
		wantWhy string
	}{
		"dry_run":          {extra: map[string]any{configuration.UnleashDryRunKey: true}, wantWhy: "dry run"},
		"integration_mode": {extra: map[string]any{configuration.UnleashIntegrationMode: true}, wantWhy: "integration mode"},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			_, res, calls := runParityWith(t, mod, prof, true, tc.extra)
			if calls != 0 {
				t.Errorf("Prepare was called %d times", calls)
			}
			if res.Schemata == nil || res.Schemata.Placed != 0 || res.Schemata.NotUsed != tc.wantWhy {
				t.Fatalf("schemata summary = %+v, want nothing placed and NotUsed %q", res.Schemata, tc.wantWhy)
			}
			out := &bytes.Buffer{}
			log.Init(out, &bytes.Buffer{})
			defer log.Reset()
			_ = report.Do(res)
			if want := "Schemata: not used (" + tc.wantWhy + ")\n"; !strings.Contains(out.String(), want) {
				t.Errorf("report does not contain %q:\n%s", want, out.String())
			}
		})
	}
}

// runParity runs the engine over mod and returns each mutant's status keyed by
// position and type, the results, and how many times Prepare was called.
func runParity(t *testing.T, mod gomodule.GoModule, prof coverage.Profile, withSchemata bool) (map[string]mutator.Status, report.Results, int) {
	t.Helper()

	return runParityWith(t, mod, prof, withSchemata, nil)
}

// runParityWith is runParity with extra settings.
func runParityWith(t *testing.T, mod gomodule.GoModule, prof coverage.Profile, withSchemata bool, extra map[string]any) (map[string]mutator.Status, report.Results, int) {
	t.Helper()
	settings := map[string]any{
		configuration.UnleashSchemataKey:   withSchemata,
		configuration.UnleashTimeoutMaxKey: "1s",
	}
	maps.Copy(settings, extra)
	// The backend's mutator set: the five the schemata prototype rewrites.
	for _, mt := range mutator.Types {
		settings[configuration.MutantTypeEnabledKey(mt)] = slices.Contains(schemaMutators, mt)
	}
	viperSet(settings)
	defer viperReset()

	wdd := workdir.NewCachedDealer(t.TempDir(), mod.Root)
	defer wdd.Clean()
	d := engine.NewExecutorDealer(mod, wdd, time.Second)
	var calls atomic.Int32
	prepare := func(ctx context.Context, m gomodule.GoModule, workDir, tags string, runnable []mutator.Mutator,
		testPkgs func(string) []string, allowance time.Duration, nullRun schemata.NullRunFunc,
	) (schemata.Plan, error) {
		calls.Add(1)

		return schemata.Prepare(ctx, m, workDir, tags, runnable, testPkgs, allowance, nullRun)
	}
	eng := engine.New(mod, engine.CodeData{Cov: prof}, d, engine.WithPrepare(prepare))
	res := eng.Run(context.Background())

	statuses := map[string]mutator.Status{}
	for _, m := range res.Mutants {
		p := m.Position()
		statuses[fmt.Sprintf("%s:%d:%d %s", p.Filename, p.Line, p.Column, m.Type())] = m.Status()
	}

	return statuses, res, int(calls.Load())
}

// parityProfile gathers the fixture's coverage the way gremlins does, without
// the chdir coverage.Run makes, which parallel tests cannot share.
func parityProfile(t *testing.T, mod gomodule.GoModule) coverage.Profile {
	t.Helper()
	out := filepath.Join(t.TempDir(), "coverage")
	cmd := exec.Command("go", "test", "-count=1", "-cover", "-coverprofile", out, "./...") //nolint:gosec // G204: test code running go on its own fixture
	cmd.Dir = mod.Root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("coverage: %v\n%s", err, b)
	}
	f, err := os.Open(out) //nolint:gosec // G304: test code reading the profile it wrote
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	profiles, err := cover.ParseProfilesFromReader(f)
	if err != nil {
		t.Fatal(err)
	}
	prof := coverage.Profile{}
	for _, p := range profiles {
		// Named as coverage names a profile's files: relative to the
		// directory Gremlins was pointed at, the frame mutant positions use.
		name, err := filepath.Rel(mod.CallingDir, strings.TrimPrefix(p.FileName, mod.Name+"/"))
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range p.Blocks {
			if b.Count == 0 {
				continue
			}
			prof[name] = append(prof[name], coverage.Block{StartLine: b.StartLine, StartCol: b.StartCol, EndLine: b.EndLine, EndCol: b.EndCol})
		}
	}

	return prof
}

// checkSharedLine requires the fixture's Mix line to hold two operators whose
// mutants the legacy run judges differently, one KILLED and one LIVED: the
// parity on it then shows that each schema id switches its own operator and
// not its neighbour on the line.
func checkSharedLine(t *testing.T, modRoot string, legacy map[string]mutator.Status) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(modRoot, "calc", "calc.go")) //nolint:gosec // G304: the fixture
	if err != nil {
		t.Fatal(err)
	}
	line := 0
	for i, l := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(l, "func Mix(") {
			line = i + 1
		}
	}
	got := map[mutator.Status]int{}
	for k, st := range legacy {
		if strings.Contains(k, fmt.Sprintf("calc.go:%d:", line)) {
			got[st]++
		}
	}
	if line == 0 || got[mutator.Killed] != 1 || got[mutator.Lived] != 1 || len(got) != 2 {
		t.Errorf("Mix (line %d) mutants by status = %v, want one KILLED and one LIVED", line, got)
	}
}

// checkKept requires the build GREMLINS_SCHEMATA_KEEP kept in dir to index
// runnable mutants, each with its id, position, type and package, and to hold
// the binary and rewritten source the index names.
func checkKept(t *testing.T, dir string, runnable int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "index.json")) //nolint:gosec // G304: the kept index
	if err != nil {
		t.Fatalf("no kept index: %v", err)
	}
	var idx struct {
		Binaries map[string]string
		Source   string
		Mutants  []struct {
			ID                      int
			Position, Type, Package string
		}
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Mutants) != runnable {
		t.Errorf("kept index has %d mutants, want %d", len(idx.Mutants), runnable)
	}
	for _, m := range idx.Mutants {
		if m.ID < 1 || !strings.Contains(m.Position, "calc.go:") || m.Type == "" || m.Package != "parity/calc" {
			t.Errorf("kept index entry %+v is incomplete", m)
		}
	}
	bin, ok := idx.Binaries["parity/calc"]
	if !ok || !strings.HasPrefix(bin, dir) {
		t.Fatalf("kept binaries %v, want parity/calc's under %s", idx.Binaries, dir)
	}
	if fi, err := os.Stat(bin); err != nil || fi.Mode()&0o100 == 0 {
		t.Errorf("kept binary %s: %v, mode %v", bin, err, fi)
	}
	if _, err := os.Stat(filepath.Join(idx.Source, "calc", "calc.go")); err != nil {
		t.Errorf("kept source lacks the rewritten calc.go: %v", err)
	}
}

// TestSchemataParityRelativeTarget runs the parity fixture the way the CLI
// does from the module root, `gremlins unleash ./calc`: gomodule.Init of a
// relative target gives a relative module root, and every placed mutant must
// still run against the schema binaries with the statuses of the legacy run.
//
// It is not parallel: it changes the working directory.
func TestSchemataParityRelativeTarget(t *testing.T) {
	t.Chdir("testdata/parity")
	mod, err := gomodule.Init("./calc")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(mod.Root) {
		t.Fatalf("gomodule.Init(./calc) gave the absolute root %s: the test proves nothing", mod.Root)
	}
	prof := parityProfile(t, mod)

	legacy, _, _ := runParity(t, mod, prof, false)
	runnable := 0
	for _, s := range legacy {
		if s != mutator.NotCovered && s != mutator.Skipped {
			runnable++
		}
	}
	if runnable == 0 {
		t.Fatalf("no runnable mutant from a relative target: %v", legacy)
	}
	withSchemata, res, _ := runParity(t, mod, prof, true)
	for _, k := range slices.Sorted(maps.Keys(legacy)) {
		if withSchemata[k] != legacy[k] {
			t.Errorf("%s: %s with --schemata, %s without", k, withSchemata[k], legacy[k])
		}
	}
	if res.Schemata == nil {
		t.Fatal("the run with --schemata has no schemata summary")
	}
	if want := (report.SchemataSummary{Placed: runnable, PerMutant: 0}); *res.Schemata != want {
		t.Errorf("schemata summary = %+v, want %+v", *res.Schemata, want)
	}
}
