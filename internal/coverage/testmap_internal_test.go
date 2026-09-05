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
	"testing"

	"github.com/google/go-cmp/cmp"
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

	t.Run("defaults to the whole module, which is what makes the map cross-package", func(t *testing.T) {
		t.Parallel()

		c := &Coverage{}
		if got := c.testMapCoverPkg(); got != wholeModule {
			t.Errorf("want %s, got %s", wholeModule, got)
		}
	})

	t.Run("honours a configured cover-pkg", func(t *testing.T) {
		t.Parallel()

		c := &Coverage{coverPkg: "./internal/..."}
		if got := c.testMapCoverPkg(); got != "./internal/..." {
			t.Errorf("want ./internal/..., got %s", got)
		}
	})
}
