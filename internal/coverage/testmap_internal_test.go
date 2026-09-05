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

	"github.com/go-gremlins/gremlins/internal/gomodule"
)

func TestParseTestList(t *testing.T) {
	t.Parallel()

	testCases := map[string]struct {
		out  string
		want map[string][]string
	}{
		// The names come first and the summary line names the package they
		// belong to, so the parse has to look ahead rather than behind.
		"attributes the names above a summary line to its package": {
			out: "TestRangeDescending\nTestRangeAscending\nok  \txpkg\t0.004s\n" +
				"TestSizeAscending\nok  \txpkg/vm\t0.005s\n",
			want: map[string][]string{
				"xpkg":    {"TestRangeDescending", "TestRangeAscending"},
				"xpkg/vm": {"TestSizeAscending"},
			},
		},
		"a package with no test files contributes nothing": {
			out:  "?   \txpkg/empty\t[no test files]\n",
			want: map[string][]string{},
		},
		"a package whose listing failed still attributes its names": {
			out:  "TestOne\nFAIL\txpkg\t0.004s\n",
			want: map[string][]string{"xpkg": {"TestOne"}},
		},
		"examples and fuzz targets are tests too": {
			out:  "ExampleAdd\nFuzzAdd\nTestAdd\nok  \txpkg\t0.004s\n",
			want: map[string][]string{"xpkg": {"ExampleAdd", "FuzzAdd", "TestAdd"}},
		},
		// Everything go prints that is not a bare test name is noise here, and
		// the parse must not mistake any of it for a test.
		"other output is not mistaken for a test name": {
			out: "go: downloading example.com v1.0.0\nBenchmarkAdd\ntesting: warning\n" +
				"TestAdd\nok  \txpkg\t0.004s\n",
			want: map[string][]string{"xpkg": {"TestAdd"}},
		},
		"no output at all yields no packages": {
			out:  "",
			want: map[string][]string{},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if diff := cmp.Diff(tc.want, parseTestList(tc.out)); diff != "" {
				t.Errorf("parseTestList() mismatch (-want +got):\n%s", diff)
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

func TestListArgs(t *testing.T) {
	t.Parallel()

	// The listing is over the whole module even when the run is scoped to one
	// package: a test that kills the mutant may live outside the scanned path,
	// and a narrower listing could not see it.
	t.Run("lists the whole module even for a scoped run", func(t *testing.T) {
		t.Parallel()

		c := &Coverage{mod: gomodule.GoModule{Name: "example.com", Root: ".", CallingDir: "internal/pkg"}}

		want := []string{"test", "-list", listPattern, wholeModule}
		if diff := cmp.Diff(want, c.listArgs()); diff != "" {
			t.Errorf("listArgs() mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("passes the build tags through", func(t *testing.T) {
		t.Parallel()

		c := &Coverage{buildTags: "tag1,tag2"}

		want := []string{"test", "-list", listPattern, "-tags", "tag1,tag2", wholeModule}
		if diff := cmp.Diff(want, c.listArgs()); diff != "" {
			t.Errorf("listArgs() mismatch (-want +got):\n%s", diff)
		}
	})
}
