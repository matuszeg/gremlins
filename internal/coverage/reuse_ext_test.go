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
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/go-gremlins/gremlins/internal/coverage"
)

// edit rewrites one of the fixture's source files, standing for the change that
// put the package in scope in the first place.
func (h *cacheHarness) edit(rel, content string) {
	h.t.Helper()

	if err := os.WriteFile(filepath.Join(h.pkgRoot, rel), []byte(content), 0o600); err != nil {
		h.t.Fatalf("cannot edit the fixture source: %v", err)
	}
}

// changedCalc reports the build ID pair that says the calc package's test binary
// was rebuilt, which is what any edit to it does.
const changedCalc = "example.com/calc=changed-by-an-edit"

func calcPos(line int) token.Position {
	return token.Position{Filename: "calc/calc.go", Line: line, Column: 1}
}

// The case the whole thing exists for. The package under mutation is the
// package that was changed, so its build ID is invalid by construction — but
// most of its tests never executed the lines that moved.
func TestAChangeReMapsOnlyTheTestsThatExecutedIt(t *testing.T) {
	h := newCacheHarness(t)

	first := h.build("TestTestMapHelperProcess", "")
	if got := first.TestsFor(calcPos(tripleEndLine)); len(got) != 0 {
		t.Fatalf("want the line below Triple uncovered before the edit, got %v", got)
	}

	h.edit("calc/calc.go", calcSourceDoubleGrown)
	second := h.build("TestTestMapHelperProcess", changedCalc)

	if diff := cmp.Diff([]string{"TestDouble"}, h.testsRun()); diff != "" {
		t.Errorf("want only the test that executed the change re-mapped (-want +got):\n%s", diff)
	}
	if second.Len() != first.Len() {
		t.Errorf("want the map still complete at %d tests, got %d", first.Len(), second.Len())
	}

	// The kept mapping has to answer about where its code is now, not where it
	// was: Triple did not change, but it moved down a line when Double grew.
	want := []coverage.TestID{{Pkg: "example.com/calc", Name: "TestTriple"}}
	if diff := cmp.Diff(want, second.TestsFor(calcPos(tripleEndLine))); diff != "" {
		t.Errorf("the kept mapping was not moved with its code (-want +got):\n%s", diff)
	}
}

// A change nothing executes is the best case, and it is not rare: most of a
// package's files are touched by a small fraction of its suite.
func TestAChangeNoTestExecutedReMapsNothing(t *testing.T) {
	h := newCacheHarness(t)

	h.build("TestTestMapHelperProcess", "")
	h.edit("vm/vm.go", vmSource+`
func Unused(v []int) int {
	return len(v) + 1
}
`)
	tm := h.build("TestTestMapHelperProcess", "example.com/vm=changed-by-an-edit")

	if got := h.testsRun(); len(got) != 0 {
		t.Errorf("want nothing re-mapped for a function nothing calls, got %v", got)
	}
	if tm.Len() != 5 {
		t.Errorf("want the map still complete, got %d tests", tm.Len())
	}
}

// A test's own body is not in any profile — coverage does not instrument test
// files — so it is attributed by name instead of by line.
func TestAChangedTestReMapsOnlyItself(t *testing.T) {
	h := newCacheHarness(t)

	h.build("TestTestMapHelperProcess", "")
	h.edit("calc/triple_test.go", `package calc

import "testing"

func TestTriple(t *testing.T) {
	if Triple(3) != 9 {
		t.Fail()
	}
}
`)
	h.build("TestTestMapHelperProcess", changedCalc)

	if diff := cmp.Diff([]string{"TestTriple"}, h.testsRun()); diff != "" {
		t.Errorf("want only the changed test re-mapped (-want +got):\n%s", diff)
	}
}

// Everything that is not an attributable declaration is all-or-nothing, because
// a line no coverage block contains can still change what the tests that
// execute the use site do.
func TestAChangeOutsideADeclarationReMapsThePackage(t *testing.T) {
	testCases := map[string]struct {
		file    string
		content string
	}{
		"a package-level constant": {"calc/calc.go", `package calc

const factor = 2

func Double(n int) int {
	return n * factor
}

func Triple(n int) int {
	return n * 3
}
`},
		"an added import": {"calc/triple_test.go", `package calc

import (
	"testing"
	_ "example.com/vm"
)

func TestTriple(t *testing.T) {
	if Triple(2) != 6 {
		t.Fail()
	}
}
`},
		"a helper in a test file": {"calc/triple_test.go", `package calc

import "testing"

func want(t *testing.T, got, expected int) {
	t.Helper()
	if got != expected {
		t.Fail()
	}
}

func TestTriple(t *testing.T) {
	want(t, Triple(2), 6)
}
`},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			h := newCacheHarness(t)
			h.build("TestTestMapHelperProcess", "")

			h.edit(tc.file, tc.content)
			h.build("TestTestMapHelperProcess", changedCalc)

			want := []string{"TestDouble", "TestTriple"}
			if diff := cmp.Diff(want, h.testsRun()); diff != "" {
				t.Errorf("want the whole package re-mapped (-want +got):\n%s", diff)
			}
		})
	}
}

// An added method changes which interfaces its receiver satisfies, which can
// send a type switch down another branch without any line of that switch
// changing. An added free function cannot: reaching it takes a call, and the
// call is a change of its own.
func TestAnAddedMethodReMapsThePackageAndAnAddedFunctionDoesNot(t *testing.T) {
	testCases := map[string]struct {
		added string
		want  []string
	}{
		"a method": {`
type counter int

func (c counter) String() string {
	return "counter"
}
`, []string{"TestDouble", "TestTriple"}},
		"a free function": {`
func Quadruple(n int) int {
	return n * 4
}
`, nil},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			h := newCacheHarness(t)
			h.build("TestTestMapHelperProcess", "")

			h.edit("calc/calc.go", calcSource+tc.added)
			h.build("TestTestMapHelperProcess", changedCalc)

			if diff := cmp.Diff(tc.want, h.testsRun()); diff != "" {
				t.Errorf("re-mapped the wrong tests (-want +got):\n%s", diff)
			}
		})
	}
}

// The build ID moves when a dependency changes, and no profile of this package
// covers a dependency — so an unchanged fingerprint under a changed build ID is
// exactly the case where the map cannot say which tests were reached.
func TestAChangeOutsideThePackageReMapsAllOfIt(t *testing.T) {
	h := newCacheHarness(t)

	h.build("TestTestMapHelperProcess", "")
	h.build("TestTestMapHelperProcess", changedCalc)

	want := []string{"TestDouble", "TestTriple"}
	if diff := cmp.Diff(want, h.testsRun()); diff != "" {
		t.Errorf("want the whole package re-mapped (-want +got):\n%s", diff)
	}
}

// A method added to a type is a global change, but changing one is not: it
// reaches only the tests that executed its lines.
func TestAChangedMethodReMapsOnlyTheTestsThatExecutedIt(t *testing.T) {
	h := newCacheHarness(t)

	withMethod := calcSource + `
type counter int

func (c counter) String() string {
	return "counter"
}
`
	h.edit("calc/calc.go", withMethod)
	h.build("TestTestMapHelperProcess", "")

	h.edit("calc/calc.go", calcSource+`
type counter int

func (c counter) String() string {
	return "a counter"
}
`)
	h.build("TestTestMapHelperProcess", changedCalc)

	// The method sits below both functions, so no test's profile reaches it.
	if got := h.testsRun(); len(got) != 0 {
		t.Errorf("want nothing re-mapped for a method no test executed, got %v", got)
	}
}
