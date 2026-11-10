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

package coverage_test

import (
	"encoding/json"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/viper"

	"github.com/go-gremlins/gremlins/internal/configuration"
	"github.com/go-gremlins/gremlins/internal/coverage"
	"github.com/go-gremlins/gremlins/internal/gomodule"
)

// The fixture names these tests read: the file the clamp is in, and the one
// test that reaches it.
const (
	clampFile       = "vm/vm.go"
	rangeDescending = "TestRangeDescending"
	rootPkg         = "example.com"
)

// A cold map runs one test at a time unless told otherwise: concurrent test
// processes multiply the memory and database connections a suite uses, and
// only its owner knows what it can take.
func TestMapWorkersDefaultsToOne(t *testing.T) {
	c := coverage.NewWithCmd(exec.Command, t.TempDir(), gomodule.GoModule{})
	if got := coverage.MapWorkersOf(c); got != 1 {
		t.Errorf("want 1, got %d", got)
	}
}

// Zero or less is one, never "as many as there are tests".
func TestMapWorkersBelowOneIsOne(t *testing.T) {
	t.Cleanup(func() { viper.Set(configuration.UnleashMapWorkersKey, nil) })
	for _, n := range []int{0, -3} {
		viper.Set(configuration.UnleashMapWorkersKey, n)
		c := coverage.NewWithCmd(exec.Command, t.TempDir(), gomodule.GoModule{})
		if got := coverage.MapWorkersOf(c); got != 1 {
			t.Errorf("map-workers %d: want 1, got %d", n, got)
		}
	}
}

func TestMapWorkersIsRead(t *testing.T) {
	t.Cleanup(func() { viper.Set(configuration.UnleashMapWorkersKey, nil) })
	viper.Set(configuration.UnleashMapWorkersKey, 4)
	c := coverage.NewWithCmd(exec.Command, t.TempDir(), gomodule.GoModule{})
	if got := coverage.MapWorkersOf(c); got != 4 {
		t.Errorf("want 4, got %d", got)
	}
}

func TestWithMapWorkersHoldsAFloorOfOne(t *testing.T) {
	c := coverage.NewWithCmd(exec.Command, t.TempDir(), gomodule.GoModule{}, coverage.WithMapWorkers(0))
	if got := coverage.MapWorkersOf(c); got != 1 {
		t.Errorf("want 1, got %d", got)
	}
	c = coverage.NewWithCmd(exec.Command, t.TempDir(), gomodule.GoModule{}, coverage.WithMapWorkers(3))
	if got := coverage.MapWorkersOf(c); got != 3 {
		t.Errorf("want 3, got %d", got)
	}
}

// maxPeak is the most runs any one run saw going at once.
func maxPeak(t *testing.T, dir string) int {
	t.Helper()

	peak := 0
	for _, l := range logLines(t, filepath.Join(dir, "peaks")) {
		n, err := strconv.Atoi(l)
		if err != nil {
			t.Fatalf("bad peak %q", l)
		}
		peak = max(peak, n)
	}

	return peak
}

func countRuns(t *testing.T, logPath, name string) int {
	t.Helper()

	n := 0
	for _, l := range logLines(t, logPath) {
		if l == "run "+name {
			n++
		}
	}

	return n
}

// N workers run a binary's tests at once, and never more than N: the root
// package holds three tests here, one more than the workers.
func TestMapWorkersRunsTestsConcurrently(t *testing.T) {
	dir := t.TempDir()
	buildMapEnv(t, "TestTestMapHelperProcess", "",
		[]string{concurrencyDirEnv + "=" + dir, extraTestEnv + "=1"}, coverage.WithMapWorkers(2))
	if peak := maxPeak(t, dir); peak != 2 {
		t.Errorf("want 2 tests running at once, saw %d", peak)
	}
}

func TestOneMapWorkerRunsTestsOneAtATime(t *testing.T) {
	dir := t.TempDir()
	buildMapEnv(t, "TestTestMapHelperProcess", "", []string{concurrencyDirEnv + "=" + dir})
	if peak := maxPeak(t, dir); peak != 1 {
		t.Errorf("want 1 test running at once, saw %d", peak)
	}
}

// cacheWithoutTimes is every cache file a map writes, decoded, without the
// times it measured: those differ from run to run whatever the order.
func cacheWithoutTimes(t *testing.T, opts ...coverage.Option) map[string]map[string]any {
	t.Helper()

	dir := t.TempDir()
	buildMapEnv(t, "TestTestMapHelperProcess", "", nil, append(opts, coverage.WithTestMapCacheDir(dir))...)
	out := map[string]map[string]any{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: the test's own temp dir
		if err != nil {
			return err
		}
		var entry map[string]any
		if err := json.Unmarshal(data, &entry); err != nil {
			return err
		}
		delete(entry, "durations")
		delete(entry, "compiled")
		rel, _ := filepath.Rel(dir, path)
		out[rel] = entry

		return nil
	})
	if err != nil {
		t.Fatalf("cannot read the cache: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the map wrote no cache")
	}

	return out
}

// The same inputs give the same map with any number of workers: in particular
// no test is credited with another's profile. (The entry is name-keyed maps
// and sorted slices throughout, so completion order could not reorder it.)
func TestMapWorkersWriteTheSameCache(t *testing.T) {
	serial := cacheWithoutTimes(t, coverage.WithMapWorkers(1))
	parallel := cacheWithoutTimes(t, coverage.WithMapWorkers(4))
	if diff := cmp.Diff(serial, parallel); diff != "" {
		t.Errorf("cache differs (-serial +parallel):\n%s", diff)
	}
}

// A test that fails while others run is re-run alone, and its alone pass is
// what is recorded.
func TestAFailureBesideOtherTestsIsRerunAlone(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "invocations")
	tm := buildMapEnv(t, "TestTestMapHelperProcess", logPath,
		[]string{failOnceEnv + "=TestRangeAscending"}, coverage.WithMapWorkers(2))
	if !tm.Mapped("example.com") {
		t.Fatal("want the package mapped from the alone re-run")
	}
	if n := countRuns(t, logPath, "TestRangeAscending"); n != 2 {
		t.Errorf("want TestRangeAscending run twice, got %d", n)
	}
	if got := tm.Len(); got != 5 {
		t.Errorf("want 5 tests mapped, got %d", got)
	}
}

// One worker re-runs nothing: a failure leaves the package unmapped, exactly
// as before --map-workers.
func TestOneMapWorkerDoesNotRerunAFailure(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "invocations")
	tm := buildMapEnv(t, "TestTestMapHelperProcess", logPath, []string{failOnceEnv + "=TestRangeAscending"})
	if tm.Mapped("example.com") {
		t.Error("want the package unmapped")
	}
	if n := countRuns(t, logPath, "TestRangeAscending"); n != 1 {
		t.Errorf("want one run, got %d", n)
	}
}

// A test that fails alone too still gives its package up.
func TestAFailureThatRepeatsAloneLeavesThePackageUnmapped(t *testing.T) {
	tm := buildMap(t, "TestTestMapHelperProcessFailingTest", coverage.WithMapWorkers(2))
	if tm.Mapped("example.com") {
		t.Error("want the package unmapped")
	}
	if !tm.Mapped("example.com/vm") {
		t.Error("want the other package still mapped")
	}
}

// A test that skipped beside other tests may have skipped because of them (a
// busy port, a database it could not get): it is re-run alone, and what it
// executes alone is what is recorded. A skip records a near-empty mapping,
// which would leave the lines it really runs without it.
func TestASkipBesideOtherTestsIsRerunAlone(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "invocations")
	tm := buildMapEnv(t, "TestTestMapHelperProcess", logPath,
		[]string{skipOnceEnv + "=" + rangeDescending}, coverage.WithMapWorkers(2))
	if n := countRuns(t, logPath, rangeDescending); n != 2 {
		t.Errorf("want TestRangeDescending run twice, got %d", n)
	}
	pos := token.Position{Filename: clampFile, Line: clampedLine, Column: 3}
	want := []coverage.TestID{{Pkg: rootPkg, Name: rangeDescending}}
	if diff := cmp.Diff(want, tm.TestsFor(pos)); diff != "" {
		t.Errorf("want the alone run's mapping (-want +got):\n%s", diff)
	}
}

// One worker asks for nothing new: the skip is what that test does alone.
func TestOneMapWorkerKeepsASkip(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "invocations")
	buildMapEnv(t, "TestTestMapHelperProcess", logPath, []string{skipOnceEnv + "=" + rangeDescending})
	if n := countRuns(t, logPath, rangeDescending); n != 1 {
		t.Errorf("want one run, got %d", n)
	}
}

// Once a test has failed alone the package is lost, so the tests after it are
// not re-run alone too: a package whose every test fails would otherwise pay
// for all of them twice.
func TestNoMoreRerunsOnceThePackageIsLost(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "invocations")
	tm := buildMapEnv(t, "TestTestMapHelperProcessFailingTest", logPath,
		[]string{failOnceEnv + "=TestRangeExtra", extraTestEnv + "=1"}, coverage.WithMapWorkers(4))
	if tm.Mapped("example.com") {
		t.Error("want the package unmapped")
	}
	// TestRangeAscending fails alone too; TestRangeExtra, listed after it,
	// is then not re-run.
	if n := countRuns(t, logPath, "TestRangeExtra"); n != 1 {
		t.Errorf("want TestRangeExtra run once, got %d", n)
	}
}

// verboseRuns counts the runs that were asked for -test.v.
func verboseRuns(t *testing.T, logPath string) int {
	t.Helper()

	n := 0
	for _, l := range logLines(t, logPath) {
		if strings.HasPrefix(l, "verbose ") {
			n++
		}
	}

	return n
}

// One worker runs the command it always ran: -test.v flips testing.Verbose()
// and prints every passing test's log, so it is asked for only where a skip
// has to be seen, beside other tests.
func TestOnlyConcurrentMappingRunsVerbose(t *testing.T) {
	serial := filepath.Join(t.TempDir(), "invocations")
	buildMapEnv(t, "TestTestMapHelperProcess", serial, nil)
	if n := verboseRuns(t, serial); n != 0 {
		t.Errorf("one worker: want no run with -test.v, got %d", n)
	}
	concurrent := filepath.Join(t.TempDir(), "invocations")
	buildMapEnv(t, "TestTestMapHelperProcess", concurrent, nil, coverage.WithMapWorkers(2))
	if n := verboseRuns(t, concurrent); n != 5 {
		t.Errorf("two workers: want every run with -test.v, got %d", n)
	}
}
