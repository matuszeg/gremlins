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
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-gremlins/gremlins/internal/log"
)

// TestID identifies one top-level test function by the package that declares it
// and its name.
type TestID struct {
	Pkg  string
	Name string
}

// String returns the test in the "package.TestName" form used in reports.
func (t TestID) String() string {
	return t.Pkg + "." + t.Name
}

// TestMap records what each test in the module executed, anywhere in the module.
//
// Go's coverage profile does not attribute blocks to the test that executed
// them, so this cannot be read out of one profile: it is built by running each
// test on its own, with coverage over the whole module, and keeping the profile
// that run produced. That is why it is expensive, and why it is opt-in.
//
// The map is what makes test selection sound in both directions. Narrower: a
// test that never executes the mutated line cannot notice the mutation, so it
// does not need to run. Wider: a test in another package that does execute the
// line can notice it, and package scoping would never have run it.
type TestMap struct {
	profiles map[TestID]Profile
	mapped   map[string]struct{}
	elapsed  time.Duration
}

// Elapsed returns how long the map took to build.
func (t *TestMap) Elapsed() time.Duration {
	return t.elapsed
}

// Len returns the number of tests in the map.
func (t *TestMap) Len() int {
	return len(t.profiles)
}

// Mapped reports whether the tests of a package were mapped.
//
// A package that was not — its listing failed, or one of its tests produced no
// profile — cannot be selected from: what is missing from the map is exactly
// what would be silently skipped. Callers must fall back to running that
// package's whole suite, which is the behaviour without selection: never wrong,
// only slow.
func (t *TestMap) Mapped(pkg string) bool {
	_, ok := t.mapped[pkg]

	return ok
}

// TestsFor returns the tests that executed the given position, ordered by
// package and then name so that a report reads the same way twice.
func (t *TestMap) TestsFor(pos token.Position) []TestID {
	var found []TestID
	for id, profile := range t.profiles {
		if profile.IsCovered(pos) {
			found = append(found, id)
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].Pkg != found[j].Pkg {
			return found[i].Pkg < found[j].Pkg
		}

		return found[i].Name < found[j].Name
	})

	return found
}

// Union returns a single Profile holding every block any test executed.
//
// It is a better answer to "is this line covered" than the profile from a plain
// coverage run, which attributes a line only to the package it lives in: a line
// executed solely by another package's tests reads as uncovered there, and the
// mutants on it are never tested at all.
func (t *TestMap) Union() Profile {
	profiles := make([]Profile, 0, len(t.profiles))
	for _, p := range t.profiles {
		profiles = append(profiles, p)
	}

	return Merge(profiles...)
}

// listPattern selects the test functions a plain `go test` would run: Test, and
// also Example and Fuzz, which -run executes too. Benchmarks are deliberately
// left out, because -run does not run them and so they cannot kill a mutant.
const listPattern = "^(Test|Example|Fuzz)"

var testNameRe = regexp.MustCompile(`^(?:Test|Example|Fuzz)[\p{L}\p{N}_]*$`)

// BuildTestMap runs every test in the module on its own and records what each
// one executed.
//
// It costs one process per test, paid once before any mutant runs. A package
// whose tests cannot be mapped is left out of the map rather than half-recorded,
// so that callers can tell "this test covers nothing here" from "we do not
// know".
func (c *Coverage) BuildTestMap() (*TestMap, error) {
	start := time.Now()
	_ = os.Chdir(c.mod.Root)

	perPkg, err := c.listTests()
	if err != nil {
		return nil, err
	}

	tm := &TestMap{
		profiles: make(map[TestID]Profile),
		mapped:   make(map[string]struct{}),
	}

	total := 0
	for _, names := range perPkg {
		total += len(names)
	}
	log.Infof("Mapping %d tests to the code they execute...\n", total)

	done := 0
	for _, pkg := range sortedPackages(perPkg) {
		complete := true
		mapped := make(map[TestID]Profile, len(perPkg[pkg]))
		for _, name := range perPkg[pkg] {
			done++
			profile, err := c.profileForTest(pkg, name)
			if err != nil {
				log.Errorf("cannot map %s.%s, so %s will run its whole suite: %v\n", pkg, name, pkg, err)
				complete = false

				continue
			}
			mapped[TestID{Pkg: pkg, Name: name}] = profile
		}
		// A partial package is discarded rather than kept, so that "the map has
		// no test here" always means "no test covers this line", never "we did
		// not look". Selecting from half a package would silently skip the other
		// half; Mapped now answers for the whole of it.
		if !complete {
			continue
		}
		for id, profile := range mapped {
			tm.profiles[id] = profile
		}
		tm.mapped[pkg] = struct{}{}
	}
	tm.elapsed = time.Since(start)
	log.Infof("Mapped %d of %d tests in %s\n", tm.Len(), done, tm.elapsed)

	return tm, nil
}

func sortedPackages(perPkg map[string][]string) []string {
	pkgs := make([]string, 0, len(perPkg))
	for pkg := range perPkg {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)

	return pkgs
}

// wholeModule is the path the map is always built over, even when the run
// itself is scoped to one package. The tests that kill a mutant are not
// necessarily in the package that holds it — that is the whole point — and a
// listing narrowed to the scanned path could not see them.
const wholeModule = "./..."

func (c *Coverage) listArgs() []string {
	args := []string{"test", "-list", listPattern}
	if c.buildTags != "" {
		args = append(args, "-tags", c.buildTags)
	}

	return append(args, wholeModule)
}

func (c *Coverage) listTests() (map[string][]string, error) {
	out, err := c.cmdContext("go", c.listArgs()...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("impossible to list the tests of the module: %w\n%s", err, out)
	}

	return parseTestList(string(out)), nil
}

// parseTestList reads the output of `go test -list`, which prints the matching
// names of a package and then that package's summary line. The summary is what
// says which package the names above it belong to.
func parseTestList(out string) map[string][]string {
	perPkg := make(map[string][]string)
	var pending []string
	for _, line := range strings.Split(out, "\n") {
		if testNameRe.MatchString(line) {
			pending = append(pending, line)

			continue
		}
		pkg, ok := packageOfSummaryLine(line)
		if !ok {
			continue
		}
		if len(pending) > 0 {
			perPkg[pkg] = pending
			pending = nil
		}
	}

	return perPkg
}

func packageOfSummaryLine(line string) (string, bool) {
	fields := strings.Split(line, "\t")
	if len(fields) < 2 {
		return "", false
	}
	switch strings.TrimSpace(fields[0]) {
	case "ok", "FAIL", "?":
		return strings.TrimSpace(fields[1]), true
	default:
		return "", false
	}
}

func (c *Coverage) profileForTest(pkg, name string) (Profile, error) {
	file := filepath.Join(c.workDir, "testmap.cov")
	args := []string{"test", "-count=1"}
	if c.buildTags != "" {
		args = append(args, "-tags", c.buildTags)
	}
	args = append(args,
		"-run", "^"+regexp.QuoteMeta(name)+"$",
		"-coverpkg", c.testMapCoverPkg(),
		"-cover", "-coverprofile", file,
		pkg)

	if out, err := c.cmdContext("go", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w\n%s", err, out)
	}

	f, err := os.Open(file) //nolint:gosec // G304: the path is Gremlins' own working directory
	if err != nil {
		return nil, err
	}
	defer func(f *os.File) {
		_ = f.Close()
	}(f)

	return c.parse(f)
}

// testMapCoverPkg is what makes the map see across packages. Without it the
// profile of a test records only the package that test lives in, which is the
// package scoping the map exists to replace.
func (c *Coverage) testMapCoverPkg() string {
	if c.coverPkg != "" {
		return c.coverPkg
	}

	return wholeModule
}
