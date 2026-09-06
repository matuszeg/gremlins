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

package coverage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/go-gremlins/gremlins/internal/gomodule"
)

func TestParsePackageList(t *testing.T) {
	t.Parallel()

	testCases := map[string]struct {
		out  string
		want []testPackage
	}{
		"reads the import path, the directory and whether there are tests": {
			out: "example.com\t/src\t2\t0\nexample.com/vm\t/src/vm\t0\t1\n",
			want: []testPackage{
				{importPath: "example.com", dir: "/src", hasTests: true},
				{importPath: "example.com/vm", dir: "/src/vm", hasTests: true},
			},
		},
		// A package with no test files is not an error and not a gap: it has
		// nothing to map, which is a complete answer.
		"a package with no test files of either kind has no tests": {
			out:  "example.com/empty\t/src/empty\t0\t0\n",
			want: []testPackage{{importPath: "example.com/empty", dir: "/src/empty", hasTests: false}},
		},
		"packages come back in a stable order": {
			out: "example.com/vm\t/src/vm\t1\t0\nexample.com\t/src\t1\t0\n",
			want: []testPackage{
				{importPath: "example.com", dir: "/src", hasTests: true},
				{importPath: "example.com/vm", dir: "/src/vm", hasTests: true},
			},
		},
		// go writes build diagnostics to the same stream, and none of them have
		// the shape of a package line.
		"anything that is not a package line is skipped": {
			out: "go: downloading example.com v1.0.0\nexample.com\t/src\t1\t0\n" +
				"# example.com/broken\nexample.com/x\t/src/x\tnot-a-number\t0\n",
			want: []testPackage{{importPath: "example.com", dir: "/src", hasTests: true}},
		},
		"no output at all yields no packages": {
			out:  "",
			want: nil,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := parsePackageList(tc.out)
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(testPackage{})); diff != "" {
				t.Errorf("parsePackageList() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseTestNames(t *testing.T) {
	t.Parallel()

	testCases := map[string]struct {
		out  string
		want []string
	}{
		"keeps the names the binary listed": {
			out:  "TestOne\nExampleTwo\nFuzzThree\n",
			want: []string{"TestOne", "ExampleTwo", "FuzzThree"},
		},
		// The binary writes this to the same stream when it is coverage-built
		// but not given a GOCOVERDIR, which is exactly how it is listed here.
		"drops the GOCOVERDIR warning the binary prints while listing": {
			out:  "TestOne\nwarning: GOCOVERDIR not set, no coverage data emitted\n",
			want: []string{"TestOne"},
		},
		"drops anything else that is not a test name": {
			out:  "BenchmarkOne\nTestOne\n--- FAIL: something\n",
			want: []string{"TestOne"},
		},
		"no output at all yields no names": {
			out:  "",
			want: nil,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if diff := cmp.Diff(tc.want, parseTestNames(tc.out)); diff != "" {
				t.Errorf("parseTestNames() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTestMapCoverPkg(t *testing.T) {
	t.Parallel()

	const pkg = "example.com/internal/vm"

	// Without --cross-package a test is only ever asked about its own package's
	// code, so instrumenting the rest of the module would record coverage that
	// nothing reads.
	t.Run("covers only the package being mapped", func(t *testing.T) {
		t.Parallel()

		c := &Coverage{}
		if got := c.testMapCoverPkg(pkg); got != pkg {
			t.Errorf("want %s, got %s", pkg, got)
		}
	})

	// With it, the point is to see a test in one package executing a line in
	// another, which only whole-module instrumentation records.
	t.Run("covers the whole module for cross-package", func(t *testing.T) {
		t.Parallel()

		c := &Coverage{crossPackage: true}
		if got := c.testMapCoverPkg(pkg); got != wholeModule {
			t.Errorf("want %s, got %s", wholeModule, got)
		}
	})

	// A configured --coverpkg is the scope of the coverage GATHER, and it applies
	// here only where the two questions coincide: under --cross-package, where
	// the map has to see outside the package anyway. Gathering over ./... while
	// judging a mutant by its own package's tests is a normal and correct
	// combination, and it used to instrument every test binary against the whole
	// module — which made every binary's build ID move whenever any file in the
	// module changed, so the per-package cache never hit.
	t.Run("a configured cover-pkg applies only under cross-package", func(t *testing.T) {
		t.Parallel()

		withCross := &Coverage{coverPkg: "./internal/...", crossPackage: true}
		if got := withCross.testMapCoverPkg(pkg); got != "./internal/..." {
			t.Errorf("with --cross-package: want ./internal/..., got %s", got)
		}

		withoutCross := &Coverage{coverPkg: "./internal/..."}
		if got := withoutCross.testMapCoverPkg(pkg); got != pkg {
			t.Errorf("without --cross-package: want %s, got %s", pkg, got)
		}
	})
}

func TestMapScope(t *testing.T) {
	t.Parallel()

	mod := gomodule.GoModule{Name: "example.com", Root: ".", CallingDir: "internal/vm"}

	t.Run("maps only the scanned path when mutants stay in their package", func(t *testing.T) {
		t.Parallel()

		c := &Coverage{mod: mod}
		if got := c.mapScope(); got != "./internal/vm/..." {
			t.Errorf("want ./internal/vm/..., got %s", got)
		}
	})

	// A test that kills a cross-package mutant can be anywhere, so a listing
	// narrowed to the scanned path could not see it.
	t.Run("maps the whole module for cross-package", func(t *testing.T) {
		t.Parallel()

		c := &Coverage{mod: mod, crossPackage: true}
		if got := c.mapScope(); got != wholeModule {
			t.Errorf("want %s, got %s", wholeModule, got)
		}
	})
}

func TestCacheKeyChangesWithWhatItCovers(t *testing.T) {
	t.Parallel()

	base := cacheKey("./...", "")

	// Both of these change what a profile means, across every package at once:
	// the coverage scope decides which packages appear in it, and the build tags
	// decide which files exist at all.
	if cacheKey("./internal/...", "") == base {
		t.Error("a different coverage scope must not share a cache")
	}
	if cacheKey("./...", "integration") == base {
		t.Error("different build tags must not share a cache")
	}
	if cacheKey("./...", "") != base {
		t.Error("the same inputs must give the same key")
	}
}

func TestLoadCachedPackageIsAMissRatherThanWrong(t *testing.T) {
	t.Parallel()

	entry := cachedPackage{
		Version:    cacheVersion,
		ImportPath: "example.com/p",
		BuildID:    "id",
		Tests: map[string]Profile{
			"TestOne": {"a.go": {{StartLine: 1, StartCol: 2, EndLine: 3, EndCol: 4}}},
		},
	}

	t.Run("a package written into a directory round-trips", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		if err := entry.save(dir); err != nil {
			t.Fatalf("save() error: %v", err)
		}

		got, ok := loadCachedPackage(dir, entry.ImportPath)
		if !ok {
			t.Fatal("want the package read back, got a miss")
		}
		if diff := cmp.Diff(entry, got); diff != "" {
			t.Errorf("cache round-trip mismatch (-want +got):\n%s", diff)
		}
	})

	// Every one of these costs a re-map of one package, which is what happens
	// without a cache at all. None of them is worth failing a run for, and none
	// may yield a half-read mapping.
	unusable := map[string]string{
		"a file that is not there": "",
		"a file that is not json":  "{not json",
		"a file from another version": `{"version":999,"import_path":"example.com/p",` +
			`"build_id":"id","tests":{}}`,
		"a file naming another package": `{"version":2,"import_path":"example.com/other",` +
			`"build_id":"id","tests":{}}`,
		"a file with no tests map": `{"version":2,"import_path":"example.com/p","build_id":"id"}`,
		"a file with no build ID": `{"version":2,"import_path":"example.com/p",` +
			`"build_id":"","tests":{}}`,
	}
	for name, content := range unusable {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			if content != "" {
				if err := os.WriteFile(cacheFilePath(dir, "example.com/p"), []byte(content), 0o600); err != nil {
					t.Fatalf("cannot write the case: %v", err)
				}
			}

			if got, ok := loadCachedPackage(dir, "example.com/p"); ok {
				t.Errorf("want a miss, got %d tests", len(got.Tests))
			}
		})
	}
}

// The point of one file per package: a run that maps one package leaves every
// other package's file exactly where it was. Under the previous layout — one
// file per module, rewritten from what the run saw — this destroyed the rest.
func TestSavingOnePackageLeavesTheOthersAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, path := range []string{"example.com/a", "example.com/b"} {
		entry := cachedPackage{
			Version: cacheVersion, ImportPath: path, BuildID: "id",
			Tests: map[string]Profile{"TestOne": {"a.go": {{StartLine: 1}}}},
		}
		if err := entry.save(dir); err != nil {
			t.Fatalf("save() error: %v", err)
		}
	}

	rewritten := cachedPackage{
		Version: cacheVersion, ImportPath: "example.com/a", BuildID: "changed",
		Tests: map[string]Profile{"TestTwo": {"a.go": {{StartLine: 9}}}},
	}
	if err := rewritten.save(dir); err != nil {
		t.Fatalf("save() error: %v", err)
	}

	if got, ok := loadCachedPackage(dir, "example.com/b"); !ok || got.BuildID != "id" {
		t.Errorf("want the untouched package still cached, got %+v (ok=%t)", got, ok)
	}
	if got, _ := loadCachedPackage(dir, "example.com/a"); got.BuildID != "changed" {
		t.Errorf("want the rewritten package updated, got build ID %q", got.BuildID)
	}
}

func TestCachePathIsOutsideTheModule(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := &Coverage{cacheDir: dir, mod: gomodule.GoModule{Name: "example.com", Root: "."}}

	path, err := c.cacheDirPath("key")
	if err != nil {
		t.Fatalf("cacheDirPath() error: %v", err)
	}
	if !strings.HasPrefix(path, dir) {
		t.Errorf("want the cache under %s, got %s", dir, path)
	}

	// Two checkouts of the same module must not share a map: the same code at
	// two paths can still map differently, and the second would inherit it.
	other := &Coverage{cacheDir: dir, mod: gomodule.GoModule{Name: "example.com", Root: t.TempDir()}}
	otherPath, err := other.cacheDirPath("key")
	if err != nil {
		t.Fatalf("cacheDirPath() error: %v", err)
	}
	if path == otherPath {
		t.Error("two checkouts of the same module must not share a cache directory")
	}

	// The key names a directory rather than living inside the files, so a map
	// gathered under a different coverage scope cannot be read as this one.
	underAnotherKey, err := c.cacheDirPath("other")
	if err != nil {
		t.Fatalf("cacheDirPath() error: %v", err)
	}
	if path == underAnotherKey {
		t.Error("two cache keys must not share a directory")
	}
}

func TestCacheFilePathSeparatesImportPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a := cacheFilePath(dir, "example.com/a")
	if a == cacheFilePath(dir, "example.com/b") {
		t.Error("two import paths must not name one file")
	}
	if a != cacheFilePath(dir, "example.com/a") {
		t.Error("the same import path must name the same file")
	}
	// An import path holds separators, so it cannot be a file name as it is.
	if filepath.Dir(a) != dir {
		t.Errorf("want the file directly under %s, got %s", dir, a)
	}
}
