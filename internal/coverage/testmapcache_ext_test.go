/*
 * Copyright 2022 The Gremlins Authors
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
	"bytes"
	"encoding/json"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/go-gremlins/gremlins/internal/coverage"
	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/internal/log"
)

// cacheHarness runs the map builder repeatedly against one cache directory,
// recording which tests each run actually executed. That record is the point:
// a run served from the cache and a run that rebuilt produce the same map, and
// only the commands issued tell them apart.
type cacheHarness struct {
	t        *testing.T
	cacheDir string
	pkgRoot  string
	logPath  string
}

func newCacheHarness(t *testing.T) *cacheHarness {
	t.Helper()

	log.Init(&bytes.Buffer{}, &bytes.Buffer{})
	t.Cleanup(log.Reset)

	return &cacheHarness{
		t:        t,
		cacheDir: t.TempDir(),
		pkgRoot:  fixtureRoot(t),
		logPath:  filepath.Join(t.TempDir(), "invocations.log"),
	}
}

// build maps the module once. buildIDs decides what `go tool buildid` reports,
// as "pkg=id" pairs; an unlisted package keeps a stable default.
func (h *cacheHarness) build(helper, buildIDs string) *coverage.TestMap {
	h.t.Helper()

	return h.buildScoped(helper, buildIDs, "")
}

// buildScoped maps only one package, the way a run scoped to the package under
// edit does. An empty package maps the whole module.
func (h *cacheHarness) buildScoped(helper, buildIDs, pkg string) *coverage.TestMap {
	h.t.Helper()

	_ = os.Remove(h.logPath)
	mod := gomodule.GoModule{Name: "example.com", Root: ".", CallingDir: "."}
	cov := coverage.NewWithCmd(
		fakeGoCommandWith(helper, h.pkgRoot, buildIDs, h.logPath, pkg),
		h.t.TempDir(), mod,
		coverage.WithTestMapCacheDir(h.cacheDir))

	tm, err := cov.BuildTestMap()
	if err != nil {
		h.t.Fatalf("BuildTestMap() error: %v", err)
	}

	return tm
}

// testsRun returns the tests the last build actually executed, sorted.
func (h *cacheHarness) testsRun() []string {
	h.t.Helper()

	data, err := os.ReadFile(h.logPath)
	if err != nil {
		return nil
	}
	var run []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if name, ok := strings.CutPrefix(line, "run "); ok {
			run = append(run, name)
		}
	}
	sort.Strings(run)

	return run
}

// cacheFile is the file holding one package's mapping. It is found by reading
// the files rather than by recomputing the name, so that the test does not
// restate the layout it is checking.
func (h *cacheHarness) cacheFile(importPath string) string {
	h.t.Helper()

	for _, path := range h.cacheFiles() {
		var entry struct {
			ImportPath string `json:"import_path"`
		}
		// #nosec G304 - the path comes from a directory this test created
		data, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(data, &entry) == nil && entry.ImportPath == importPath {
			return path
		}
	}
	h.t.Fatalf("no cache file was written for %s", importPath)

	return ""
}

// cacheFiles lists the mapping files on disk, which is one per mapped package.
func (h *cacheHarness) cacheFiles() []string {
	h.t.Helper()

	var found []string
	_ = filepath.Walk(h.cacheDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".json") {
			found = append(found, path)
		}

		return nil
	})
	sort.Strings(found)

	return found
}

// blockOrder makes a union comparable. Merge keeps every distinct block but not
// a fixed order, because it reads profiles out of a map.
func blockOrder() cmp.Option {
	return cmpopts.SortSlices(func(a, b coverage.Block) bool {
		if a.StartLine != b.StartLine {
			return a.StartLine < b.StartLine
		}

		return a.StartCol < b.StartCol
	})
}

func allTests() []string {
	return []string{
		"TestDouble", "TestRangeAscending", "TestRangeDescending",
		"TestSizeAscending", "TestTriple",
	}
}

func TestMapCacheReusesUnchangedPackages(t *testing.T) {
	h := newCacheHarness(t)

	first := h.build("TestTestMapHelperProcess", "")
	if diff := cmp.Diff(allTests(), h.testsRun()); diff != "" {
		t.Fatalf("the first build should run every test (-want +got):\n%s", diff)
	}

	second := h.build("TestTestMapHelperProcess", "")

	// Nothing changed, so nothing needed running — and the map is the same map.
	if got := h.testsRun(); len(got) != 0 {
		t.Errorf("want no test executed on the second build, got %v", got)
	}
	if diff := cmp.Diff(first.Union(), second.Union(), blockOrder()); diff != "" {
		t.Errorf("the cached map covers different code (-want +got):\n%s", diff)
	}
	if first.Len() != second.Len() {
		t.Errorf("want %d tests from the cache, got %d", first.Len(), second.Len())
	}
	if !second.Mapped("example.com") || !second.Mapped("example.com/vm") {
		t.Error("a package served from the cache must still report as mapped")
	}
}

// The build ID is computed over the package's own source AND every dependency's,
// so a change anywhere beneath a package invalidates exactly the binaries that
// link it — which is what this stands for.
func TestMapCacheRemapsOnlyThePackageWhoseBuildIDChanged(t *testing.T) {
	h := newCacheHarness(t)

	h.build("TestTestMapHelperProcess", "")
	h.build("TestTestMapHelperProcess", "example.com/vm=changed-by-a-dependency")

	want := []string{"TestSizeAscending"}
	if diff := cmp.Diff(want, h.testsRun()); diff != "" {
		t.Errorf("only the changed package should be re-mapped (-want +got):\n%s", diff)
	}
}

func TestMapCacheDoesNotKeepAPackageItCouldNotMapWhole(t *testing.T) {
	h := newCacheHarness(t)

	first := h.build("TestTestMapHelperProcessFailingTest", "")
	if first.Mapped("example.com") {
		t.Fatal("the package with an unrunnable test should not be mapped")
	}

	// Caching a half-mapped package would make the gap permanent, so the next
	// run has to try it again.
	h.build("TestTestMapHelperProcessFailingTest", "")

	if got := h.testsRun(); !contains(got, "TestRangeDescending") {
		t.Errorf("want the unmappable package retried, got %v", got)
	}
}

func TestMapCacheRebuildsWhenTheFileIsUnusable(t *testing.T) {
	testCases := map[string]func(path string) error{
		"corrupt json": func(path string) error {
			return os.WriteFile(path, []byte("{not json"), 0o600)
		},
		"a cache written by another version": func(path string) error {
			return os.WriteFile(path, []byte(`{"version":999,"import_path":"example.com",`+
				`"build_id":"x","tests":{}}`), 0o600)
		},
		"a cache file naming another package": func(path string) error {
			return os.WriteFile(path, []byte(`{"version":2,"import_path":"example.com/elsewhere",`+
				`"build_id":"x","tests":{}}`), 0o600)
		},
		"no cache file at all": os.Remove,
	}

	for name, corrupt := range testCases {
		t.Run(name, func(t *testing.T) {
			h := newCacheHarness(t)
			h.build("TestTestMapHelperProcess", "")

			if err := corrupt(h.cacheFile("example.com")); err != nil {
				t.Fatalf("cannot set up the case: %v", err)
			}

			tm := h.build("TestTestMapHelperProcess", "")

			// A file that cannot be read costs a re-map of its package, never a
			// wrong map and never a failed run. The other package still hits.
			want := []string{"TestRangeAscending", "TestRangeDescending"}
			if diff := cmp.Diff(want, h.testsRun()); diff != "" {
				t.Errorf("want the unusable package re-mapped (-want +got):\n%s", diff)
			}
			if tm.Len() != 5 {
				t.Errorf("want the full map after the rebuild, got %d tests", tm.Len())
			}
		})
	}
}

func TestMapCacheSurvivesTheRoundTripExactly(t *testing.T) {
	h := newCacheHarness(t)

	first := h.build("TestTestMapHelperProcess", "")
	second := h.build("TestTestMapHelperProcess", "")

	pos := token.Position{Filename: "vm/vm.go", Line: clampedLine, Column: 3}
	if diff := cmp.Diff(first.TestsFor(pos), second.TestsFor(pos)); diff != "" {
		t.Errorf("the cached map answers differently (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(first.Union(), second.Union(), blockOrder()); diff != "" {
		t.Errorf("the cached union differs (-want +got):\n%s", diff)
	}
}

// The defect this layout exists to remove: a run scoped to one package used to
// rewrite the module's single cache file from what it had seen, discarding
// every package it never looked at.
func TestAScopedRunDoesNotEvictTheRestOfTheModule(t *testing.T) {
	h := newCacheHarness(t)

	h.build("TestTestMapHelperProcess", "")
	if got := h.cacheFiles(); len(got) != 3 {
		t.Fatalf("want one file per package with tests, got %d", len(got))
	}

	scoped := h.buildScoped("TestTestMapHelperProcess", "", "example.com/vm")
	if got := scoped.Len(); got != 1 {
		t.Errorf("want the scoped run to map its one package, got %d tests", got)
	}
	if got := h.cacheFiles(); len(got) != 3 {
		t.Errorf("want the other packages' files left alone, got %d files", len(got))
	}

	// The whole-module run that follows is the one that pays for eviction, so
	// it is the one that proves there was none.
	h.build("TestTestMapHelperProcess", "")
	if got := h.testsRun(); len(got) != 0 {
		t.Errorf("want every package still served from the cache, got %v re-run", got)
	}
}

// Two runs scoped to different packages of one module used to share a file, so
// the second save won and the first run's work was lost.
func TestTwoScopedRunsDoNotClobberEachOther(t *testing.T) {
	h := newCacheHarness(t)

	h.buildScoped("TestTestMapHelperProcess", "", "example.com")
	h.buildScoped("TestTestMapHelperProcess", "", "example.com/vm")
	h.buildScoped("TestTestMapHelperProcess", "", "example.com/calc")

	h.build("TestTestMapHelperProcess", "")
	if got := h.testsRun(); len(got) != 0 {
		t.Errorf("want both scoped runs' work kept, got %v re-run", got)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}

	return false
}
