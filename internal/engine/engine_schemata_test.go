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
	for _, s := range []mutator.Status{mutator.Killed, mutator.Lived, mutator.NotCovered, mutator.Skipped, mutator.RunTimedOut} {
		if seen[s] == 0 {
			t.Errorf("the fixture produced no %s mutant without --schemata: %v", s, legacy)
		}
	}
	runnable := len(legacy) - seen[mutator.NotCovered] - seen[mutator.Skipped]

	withSchemata, res, calls := runParity(t, mod, prof, true)
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

// runParity runs the engine over mod and returns each mutant's status keyed by
// position and type, the results, and how many times Prepare was called.
func runParity(t *testing.T, mod gomodule.GoModule, prof coverage.Profile, withSchemata bool) (map[string]mutator.Status, report.Results, int) {
	t.Helper()
	settings := map[string]any{
		configuration.UnleashSchemataKey:   withSchemata,
		configuration.UnleashTimeoutMaxKey: "1s",
	}
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
		name := strings.TrimPrefix(p.FileName, mod.Name+"/")
		for _, b := range p.Blocks {
			if b.Count == 0 {
				continue
			}
			prof[name] = append(prof[name], coverage.Block{StartLine: b.StartLine, StartCol: b.StartCol, EndLine: b.EndLine, EndCol: b.EndCol})
		}
	}

	return prof
}
