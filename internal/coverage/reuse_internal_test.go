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
		Fingerprint: fingerprint{Shell: shell, Inputs: inputsHash, Decls: decls},
		Tests: map[string]Profile{
			"TestF": {"a.go": {{StartLine: 4, StartCol: 1, EndLine: 6, EndCol: 2}}},
		},
	}
}

// inputsHash stands for everything the test binary is built from besides the
// package: unchanged in every case that narrows, because a change there is one
// no profile of this package could have recorded.
const inputsHash = "inputs"

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
			now:    fingerprint{Shell: "shell", Inputs: inputsHash, Decls: base},
		},
		// A const, a package-level var, a type, a struct tag, an import, a test
		// helper, a non-Go file: none of them is a line any profile holds.
		"the shell moved": {
			cached: oneTest(base, "shell"),
			now:    fingerprint{Shell: "other", Inputs: inputsHash, Decls: base},
		},
		// init runs before every test in the binary.
		"an init changed": {
			cached: oneTest(map[string]declPrint{
				"a.go:F":    base["a.go:F"],
				"a.go:init": decl("a.go", 13, 15, "i", kindInit),
			}, "shell"),
			now: fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
				"a.go:F":    base["a.go:F"],
				"a.go:init": decl("a.go", 13, 15, "i2", kindInit),
			}},
		},
		"an init was added": {
			cached: oneTest(base, "shell"),
			now: fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
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
			now: fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{"a.go:F": base["a.go:F"]}},
		},
		"a method was added": {
			cached: oneTest(base, "shell"),
			now: fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
				"a.go:F":     base["a.go:F"],
				"a.go:G":     base["a.go:G"],
				"a.go:T.Str": decl("a.go", 13, 15, "m", kindMethod),
			}},
		},
		// A new package-level function named like a predeclared identifier
		// rebinds every existing use of that identifier without a line of the
		// use changing: `min(x, 10)` calls the new function from then on.
		"an added function shadows a predeclared identifier": {
			cached: oneTest(base, "shell"),
			now: fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
				"a.go:F":   base["a.go:F"],
				"a.go:G":   base["a.go:G"],
				"a.go:min": decl("a.go", 13, 15, "m", ""),
			}},
		},
		// A dependency moved, alone or alongside a change here. Its lines are
		// in no profile of this package, so which mappings it reached cannot be
		// worked out — and the build ID, which folds the two together, cannot
		// tell one from the other.
		"the dependencies moved too": {
			cached: oneTest(base, "shell"),
			now: fingerprint{Shell: "shell", Inputs: "other-inputs", Decls: map[string]declPrint{
				"a.go:F": decl("a.go", 3, 7, "f2", ""),
				"a.go:G": base["a.go:G"],
			}},
		},
		"nothing recorded what it was built from": {
			cached: cachedPackage{
				Fingerprint: fingerprint{Shell: "shell", Decls: base},
				Tests:       map[string]Profile{"TestF": {"a.go": {{StartLine: 4, EndLine: 6}}}},
			},
			now: fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
				"a.go:F": decl("a.go", 3, 7, "f2", ""),
				"a.go:G": base["a.go:G"],
			}},
		},
		// Positions are paired by order, which only means anything while the
		// sequence is the same one.
		"the shell declarations no longer line up": {
			cached: cachedPackage{
				Fingerprint: fingerprint{
					Shell:  "shell",
					Inputs: inputsHash,
					Decls:  base,
					Others: map[string][]declPrint{"a.go": {{File: "a.go", Start: 1, End: 1}}},
				},
				Tests: map[string]Profile{"TestF": {"a.go": {{StartLine: 4, EndLine: 6}}}},
			},
			now: fingerprint{Shell: "shell", Inputs: inputsHash, Others: map[string][]declPrint{}, Decls: map[string]declPrint{
				"a.go:F": decl("a.go", 3, 7, "f2", ""),
				"a.go:G": base["a.go:G"],
			}},
		},
		// The profile and the fingerprint disagree about the package: the
		// mapping covers a file no declaration accounts for, so where its blocks
		// have moved to cannot be worked out.
		"a kept block belongs to no declaration": {
			cached: cachedPackage{
				Fingerprint: fingerprint{Shell: "shell", Inputs: inputsHash, Decls: base},
				Tests: map[string]Profile{
					"TestF": {"elsewhere.go": {{StartLine: 4, EndLine: 6}}},
				},
			},
			now: fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
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
	was := fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
		"a.go:F": decl("a.go", 3, 7, "f", ""),
		"a.go:G": decl("a.go", 9, 11, "g", ""),
	}}
	now := fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
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

// A package-level var holding a function literal is instrumented like any other
// code, and it is in the shell — so without its position a mapping that reached
// it could never be kept.
func TestReusableMovesTheDeclarationsTheShellHolds(t *testing.T) {
	t.Parallel()

	was := fingerprint{
		Shell:  "shell",
		Inputs: inputsHash,
		Decls:  map[string]declPrint{"a.go:F": decl("a.go", 3, 7, "f", "")},
		Others: map[string][]declPrint{"a.go": {{File: "a.go", Start: 9, End: 12}}},
	}
	now := fingerprint{
		Shell:  "shell",
		Inputs: inputsHash,
		Decls:  map[string]declPrint{"a.go:F": decl("a.go", 3, 9, "f2", "")},
		Others: map[string][]declPrint{"a.go": {{File: "a.go", Start: 11, End: 14}}},
	}
	cached := cachedPackage{
		Fingerprint: was,
		Tests: map[string]Profile{
			"TestVar": {"a.go": {{StartLine: 10, StartCol: 1, EndLine: 10, EndCol: 9}}},
		},
	}

	kept, ok := reusable(cached, now)
	if !ok {
		t.Fatal("want the mapping kept, got a whole-package re-map")
	}
	want := map[string]Profile{
		"TestVar": {"a.go": {{StartLine: 12, StartCol: 1, EndLine: 12, EndCol: 9}}},
	}
	if diff := cmp.Diff(want, kept); diff != "" {
		t.Errorf("the shell declaration was not moved with the rest (-want +got):\n%s", diff)
	}
}

// A test's own lines are in no profile — coverage does not instrument test
// files — so a changed test is named rather than located.
func TestReusableDropsAChangedTestByName(t *testing.T) {
	t.Parallel()

	was := fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
		"a.go:F":     decl("a.go", 3, 7, "f", ""),
		"test:TestF": {Hash: "t", File: "a_test.go", Test: "TestF", Start: 5, End: 9},
	}}
	now := fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
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

// An unchanged fingerprint under a moved build ID used to mean a dependency had
// changed, because nothing else could move it. Inputs now records every
// dependency, the toolchain and the build environment, so with those equal too
// what is left is the checkout path the binary embeds — and every mapping is
// still the answer.
func TestReusableKeepsEverythingWhenOnlyTheBuildIDMoved(t *testing.T) {
	t.Parallel()

	decls := map[string]declPrint{
		"a.go:F": decl("a.go", 3, 7, "f", ""),
		"a.go:G": decl("a.go", 9, 11, "g", ""),
	}
	cached := cachedPackage{
		Fingerprint: fingerprint{Shell: "shell", Inputs: inputsHash, Decls: decls},
		Tests: map[string]Profile{
			"TestF": {"a.go": {{StartLine: 4, StartCol: 1, EndLine: 6, EndCol: 2}}},
			"TestG": {"a.go": {{StartLine: 10, StartCol: 1, EndLine: 10, EndCol: 2}}},
		},
	}

	kept, ok := reusable(cached, fingerprint{Shell: "shell", Inputs: inputsHash, Decls: decls})
	if !ok {
		t.Fatal("want every mapping kept, got a whole-package re-map")
	}
	if diff := cmp.Diff(cached.Tests, kept); diff != "" {
		t.Errorf("want every mapping kept where it was (-want +got):\n%s", diff)
	}
}

// The shell drops whitespace between declarations, so blank lines added above a
// function move it without moving the shell or any declaration's hash. Nothing
// changed in that case either, but the lines did, and a mapping kept at its old
// numbers would answer about the wrong ones.
func TestReusableMovesEverythingWhenOnlyWhitespaceMoved(t *testing.T) {
	t.Parallel()

	cached := cachedPackage{
		Fingerprint: fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
			"a.go:F": decl("a.go", 3, 7, "f", ""),
		}},
		Tests: map[string]Profile{
			"TestF": {"a.go": {{StartLine: 4, StartCol: 1, EndLine: 6, EndCol: 2}}},
		},
	}
	now := fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
		"a.go:F": decl("a.go", 5, 9, "f", ""),
	}}

	kept, ok := reusable(cached, now)
	if !ok {
		t.Fatal("want the mapping kept, got a whole-package re-map")
	}
	want := map[string]Profile{
		"TestF": {"a.go": {{StartLine: 6, StartCol: 1, EndLine: 8, EndCol: 2}}},
	}
	if diff := cmp.Diff(want, kept); diff != "" {
		t.Errorf("the mapping was not moved with its code (-want +got):\n%s", diff)
	}
}

// Only a predeclared name is rebound silently. Any other new name either
// conflicts with something already declared, which does not compile, or is
// reached only through a call site that is a change of its own.
func TestReusableKeepsMappingsAcrossAnAddedOrdinaryFunction(t *testing.T) {
	t.Parallel()

	base := map[string]declPrint{"a.go:F": decl("a.go", 3, 7, "f", "")}
	now := fingerprint{Shell: "shell", Inputs: inputsHash, Decls: map[string]declPrint{
		"a.go:F":       base["a.go:F"],
		"a.go:Minimum": decl("a.go", 9, 11, "m", ""),
	}}

	kept, ok := reusable(oneTest(base, "shell"), now)
	if !ok {
		t.Fatal("want the mapping kept, got a whole-package re-map")
	}
	if _, still := kept["TestF"]; !still {
		t.Error("want TestF kept")
	}
}
