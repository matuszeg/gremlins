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

// oneTest is a package with a single mapping covering lines 4 to 6 of a.go,
// which is the body of the declaration keyed "a.go:F".
func oneTest(decls map[string]declPrint, shell string) cachedPackage {
	return cachedPackage{
		Fingerprint: fingerprint{Shell: shell, Decls: decls},
		Tests: map[string]Profile{
			"TestF": {"a.go": {{StartLine: 4, StartCol: 1, EndLine: 6, EndCol: 2}}},
		},
	}
}

func decl(file string, start, end int, hash, kind string) declPrint {
	return declPrint{Hash: hash, File: file, Start: start, End: end, Kind: kind}
}

// Everything here re-maps the whole package. The direction matters: a mapping
// wrongly kept means a test that could kill a mutant is never run, so the
// mutant reports LIVED and the gate goes red on something nobody can reproduce.
func TestReusableRefusesWhatItCannotAttribute(t *testing.T) {
	t.Parallel()

	base := map[string]declPrint{
		"a.go:F": decl("a.go", 3, 7, "f", ""),
		"a.go:G": decl("a.go", 9, 11, "g", ""),
	}

	testCases := map[string]struct {
		cached cachedPackage
		now    fingerprint
	}{
		// An entry from before the fingerprint existed, or one whose package
		// could not be read, says nothing about what changed.
		"no fingerprint at all": {
			cached: oneTest(base, ""),
			now:    fingerprint{Shell: "shell", Decls: base},
		},
		// A const, a package-level var, a type, a struct tag, an import, a test
		// helper, a non-Go file: none of them is a line any profile holds.
		"the shell moved": {
			cached: oneTest(base, "shell"),
			now:    fingerprint{Shell: "other", Decls: base},
		},
		// Nothing in the package changed, yet the build ID did — so the change
		// was in a dependency, which no profile of this package covers.
		"nothing in the package changed": {
			cached: oneTest(base, "shell"),
			now:    fingerprint{Shell: "shell", Decls: base},
		},
		// init runs before every test in the binary.
		"an init changed": {
			cached: oneTest(map[string]declPrint{
				"a.go:F":    base["a.go:F"],
				"a.go:init": decl("a.go", 13, 15, "i", kindInit),
			}, "shell"),
			now: fingerprint{Shell: "shell", Decls: map[string]declPrint{
				"a.go:F":    base["a.go:F"],
				"a.go:init": decl("a.go", 13, 15, "i2", kindInit),
			}},
		},
		"an init was added": {
			cached: oneTest(base, "shell"),
			now: fingerprint{Shell: "shell", Decls: map[string]declPrint{
				"a.go:F":    base["a.go:F"],
				"a.go:G":    base["a.go:G"],
				"a.go:init": decl("a.go", 13, 15, "i", kindInit),
			}},
		},
		// Adding or removing a method changes which interfaces its receiver
		// satisfies, which can redirect a type switch that did not change.
		"a method was removed": {
			cached: oneTest(map[string]declPrint{
				"a.go:F":     base["a.go:F"],
				"a.go:T.Str": decl("a.go", 13, 15, "m", kindMethod),
			}, "shell"),
			now: fingerprint{Shell: "shell", Decls: map[string]declPrint{"a.go:F": base["a.go:F"]}},
		},
		"a method was added": {
			cached: oneTest(base, "shell"),
			now: fingerprint{Shell: "shell", Decls: map[string]declPrint{
				"a.go:F":     base["a.go:F"],
				"a.go:G":     base["a.go:G"],
				"a.go:T.Str": decl("a.go", 13, 15, "m", kindMethod),
			}},
		},
		// The profile and the fingerprint disagree about the package: the
		// mapping covers a file no declaration accounts for, so where its blocks
		// have moved to cannot be worked out.
		"a kept block belongs to no declaration": {
			cached: cachedPackage{
				Fingerprint: fingerprint{Shell: "shell", Decls: base},
				Tests: map[string]Profile{
					"TestF": {"elsewhere.go": {{StartLine: 4, EndLine: 6}}},
				},
			},
			now: fingerprint{Shell: "shell", Decls: map[string]declPrint{
				"a.go:F": decl("a.go", 3, 7, "f2", ""),
				"a.go:G": base["a.go:G"],
			}},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if kept, ok := reusable(tc.cached, tc.now); ok {
				t.Errorf("want the whole package re-mapped, got %d mappings kept", len(kept))
			}
		})
	}
}

func TestReusableKeepsWhatTheChangeCouldNotReach(t *testing.T) {
	t.Parallel()

	// F changed and grew by two lines; G did not change and moved with it.
	was := fingerprint{Shell: "shell", Decls: map[string]declPrint{
		"a.go:F": decl("a.go", 3, 7, "f", ""),
		"a.go:G": decl("a.go", 9, 11, "g", ""),
	}}
	now := fingerprint{Shell: "shell", Decls: map[string]declPrint{
		"a.go:F": decl("a.go", 3, 9, "f2", ""),
		"a.go:G": decl("a.go", 11, 13, "g", ""),
	}}
	cached := cachedPackage{
		Fingerprint: was,
		Tests: map[string]Profile{
			"TestF": {"a.go": {{StartLine: 4, StartCol: 1, EndLine: 6, EndCol: 2}}},
			"TestG": {"a.go": {{StartLine: 10, StartCol: 1, EndLine: 10, EndCol: 2}}},
		},
	}

	kept, ok := reusable(cached, now)
	if !ok {
		t.Fatal("want the untouched mapping kept, got a whole-package re-map")
	}

	want := map[string]Profile{
		"TestG": {"a.go": {{StartLine: 12, StartCol: 1, EndLine: 12, EndCol: 2}}},
	}
	if diff := cmp.Diff(want, kept); diff != "" {
		t.Errorf("kept the wrong mappings, or in the wrong place (-want +got):\n%s", diff)
	}
}

// A test's own lines are in no profile — coverage does not instrument test
// files — so a changed test is named rather than located.
func TestReusableDropsAChangedTestByName(t *testing.T) {
	t.Parallel()

	was := fingerprint{Shell: "shell", Decls: map[string]declPrint{
		"a.go:F":     decl("a.go", 3, 7, "f", ""),
		"test:TestF": {Hash: "t", File: "a_test.go", Test: "TestF", Start: 5, End: 9},
	}}
	now := fingerprint{Shell: "shell", Decls: map[string]declPrint{
		"a.go:F":     decl("a.go", 3, 7, "f", ""),
		"test:TestF": {Hash: "t2", File: "a_test.go", Test: "TestF", Start: 5, End: 9},
	}}

	kept, ok := reusable(oneTest(was.Decls, "shell"), now)
	if !ok {
		t.Fatal("want a narrowed re-map, got a whole-package one")
	}
	if _, still := kept["TestF"]; still {
		t.Error("want the changed test dropped from what is kept")
	}
}
