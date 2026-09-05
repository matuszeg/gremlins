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
	"fmt"
	"go/token"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/go-gremlins/gremlins/internal/coverage"
	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/internal/log"
)

// The fixture is the shape that makes cross-package selection worth having: the
// clamp on vm.go:7 is executed only by a test in the root package, and the vm
// package's own test never reaches it.
const (
	clampedLine   = 7
	uncoveredLine = 20
)

func newTestMap(t *testing.T) *coverage.TestMap {
	t.Helper()

	log.Init(&bytes.Buffer{}, &bytes.Buffer{})
	t.Cleanup(log.Reset)

	mod := gomodule.GoModule{Name: "example.com", Root: ".", CallingDir: "."}
	cov := coverage.NewWithCmd(fakeTestMapCommand, t.TempDir(), mod)

	tm, err := cov.BuildTestMap()
	if err != nil {
		t.Fatalf("BuildTestMap() error: %v", err)
	}

	return tm
}

func TestBuildTestMap(t *testing.T) {
	tm := newTestMap(t)

	t.Run("maps every test of every package", func(t *testing.T) {
		if got := tm.Len(); got != 3 {
			t.Errorf("want 3 tests mapped, got %d", got)
		}
	})

	t.Run("finds the test in another package that executes the line", func(t *testing.T) {
		pos := token.Position{Filename: "vm/vm.go", Line: clampedLine, Column: 3}

		want := []coverage.TestID{{Pkg: "example.com", Name: "TestRangeDescending"}}
		if diff := cmp.Diff(want, tm.TestsFor(pos)); diff != "" {
			t.Errorf("TestsFor() mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("does not select a test that never executes the line", func(t *testing.T) {
		pos := token.Position{Filename: "vm/vm.go", Line: uncoveredLine, Column: 3}

		if got := tm.TestsFor(pos); len(got) != 0 {
			t.Errorf("want no tests, got %v", got)
		}
	})

	t.Run("the union holds what any test executed, once", func(t *testing.T) {
		union := tm.Union()

		if !union.IsCovered(token.Position{Filename: "vm/vm.go", Line: clampedLine, Column: 3}) {
			t.Error("expected the union to cover the line only one test executes")
		}
		if got := len(union["vm/vm.go"]); got != 2 {
			t.Errorf("want the two distinct blocks of vm.go, got %d", got)
		}
	})

	t.Run("a package whose tests all mapped can be selected from", func(t *testing.T) {
		if !tm.Mapped("example.com") || !tm.Mapped("example.com/vm") {
			t.Error("expected both packages to be mapped")
		}
	})

	t.Run("a package that was never listed is not mapped", func(t *testing.T) {
		if tm.Mapped("example.com/absent") {
			t.Error("expected an unknown package to report as unmapped")
		}
	})
}

func TestBuildTestMapLeavesAPackageUnmappedWhenATestCannotBeRun(t *testing.T) {
	log.Init(&bytes.Buffer{}, &bytes.Buffer{})
	defer log.Reset()

	mod := gomodule.GoModule{Name: "example.com", Root: ".", CallingDir: "."}
	cov := coverage.NewWithCmd(fakeTestMapCommandWithFailure, t.TempDir(), mod)

	tm, err := cov.BuildTestMap()
	if err != nil {
		t.Fatalf("BuildTestMap() error: %v", err)
	}

	// Half a package is worse than none of it: what is missing from the map is
	// exactly what selection would silently skip, so the package must fall back
	// to its whole suite.
	if tm.Mapped("example.com") {
		t.Error("expected the package holding the unrunnable test to be unmapped")
	}
	if !tm.Mapped("example.com/vm") {
		t.Error("expected the other package to still be mapped")
	}
	// Its mappable tests are discarded with it: a half-recorded package would
	// make "no test covers this line" indistinguishable from "we did not look".
	if got := tm.Len(); got != 1 {
		t.Errorf("want only the mapped package's single test, got %d", got)
	}
}

func TestBuildTestMapFailsWhenTheModuleCannotBeListed(t *testing.T) {
	log.Init(&bytes.Buffer{}, &bytes.Buffer{})
	defer log.Reset()

	mod := gomodule.GoModule{Name: "example.com", Root: ".", CallingDir: "."}
	cov := coverage.NewWithCmd(fakeTestMapCommandListFailure, t.TempDir(), mod)

	if _, err := cov.BuildTestMap(); err == nil {
		t.Error("expected an error when the tests cannot be listed")
	}
}

const (
	testMapListing = "TestRangeDescending\nTestRangeAscending\nok  \texample.com\t0.004s\n" +
		"TestSizeAscending\nok  \texample.com/vm\t0.005s\n"

	// TestRangeDescending is the only test that reaches the clamp.
	profileRangeDescending = "mode: set\n" +
		"example.com/root.go:6.26,6.50 1 1\n" +
		"example.com/vm/vm.go:4.29,6.15 2 1\n" +
		"example.com/vm/vm.go:6.15,8.3 1 1\n"
	profileRangeAscending = "mode: set\n" +
		"example.com/root.go:6.26,6.50 1 1\n" +
		"example.com/vm/vm.go:4.29,6.15 2 1\n"
	profileSizeAscending = "mode: set\n" +
		"example.com/vm/vm.go:4.29,6.15 2 1\n"
)

func fakeTestMapCommand(command string, args ...string) *exec.Cmd {
	return testMapHelper("TestTestMapHelperProcess", command, args...)
}

func fakeTestMapCommandWithFailure(command string, args ...string) *exec.Cmd {
	return testMapHelper("TestTestMapHelperProcessFailingTest", command, args...)
}

func fakeTestMapCommandListFailure(command string, args ...string) *exec.Cmd {
	return testMapHelper("TestTestMapHelperProcessListFailure", command, args...)
}

func testMapHelper(run, command string, args ...string) *exec.Cmd {
	cs := []string{"-test.run=" + run, "--", command}
	cs = append(cs, args...)
	// #nosec G204 G702 - We are in tests, we don't care
	cmd := exec.Command(os.Args[0], cs...)
	cmd.Env = []string{"GO_TEST_PROCESS=1"}

	return cmd
}

// TestTestMapHelperProcess stands in for the go command: it answers -list with a
// canned listing, and writes the profile a per-test coverage run would produce.
func TestTestMapHelperProcess(t *testing.T) {
	if os.Getenv("GO_TEST_PROCESS") != "1" {
		return
	}
	respondAsGo(t, "")
}

func TestTestMapHelperProcessFailingTest(t *testing.T) {
	if os.Getenv("GO_TEST_PROCESS") != "1" {
		return
	}
	respondAsGo(t, "TestRangeAscending")
}

func TestTestMapHelperProcessListFailure(t *testing.T) {
	if os.Getenv("GO_TEST_PROCESS") != "1" {
		return
	}
	if hasFlag(os.Args, "-list") {
		fmt.Fprintln(os.Stderr, "cannot load package")
		os.Exit(1) // skipcq: RVV-A0003
	}
	respondAsGo(t, "")
}

func respondAsGo(t *testing.T, failing string) {
	t.Helper()

	if hasFlag(os.Args, "-list") {
		fmt.Fprint(os.Stdout, testMapListing)
		os.Exit(0) // skipcq: RVV-A0003
	}

	run := strings.Trim(flagValue(os.Args, "-run"), "^$")
	if run == failing {
		fmt.Fprintln(os.Stderr, "--- FAIL: "+run)
		os.Exit(1) // skipcq: RVV-A0003
	}

	profiles := map[string]string{
		"TestRangeDescending": profileRangeDescending,
		"TestRangeAscending":  profileRangeAscending,
		"TestSizeAscending":   profileSizeAscending,
	}
	profile, ok := profiles[run]
	if !ok {
		fmt.Fprintln(os.Stderr, "unexpected -run "+run)
		os.Exit(1) // skipcq: RVV-A0003
	}
	// #nosec G703 - the path comes from the arguments this test process gave itself
	if err := os.WriteFile(flagValue(os.Args, "-coverprofile"), []byte(profile), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1) // skipcq: RVV-A0003
	}
	os.Exit(0) // skipcq: RVV-A0003
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}

	return false
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}

	return ""
}
