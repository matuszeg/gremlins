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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/internal/log"
)

// The differential suite is the one that can see a mapping kept wrongly. Every
// other test of reuse drives a fake go command with canned profiles, so a kept
// mapping is checked against what the fake was told to say, never against what
// the test really executes after the change. Here the toolchain is real: each
// case maps a small module, applies an edit, maps it again from the cache, and
// maps it once more from nothing. The two maps after the edit must agree test
// for test, profile and dependency keys both — a mapping the reuse kept that a
// fresh run would not produce is exactly the false LIVED this whole mechanism
// has to avoid.
//
// Agreement alone would be satisfied by re-mapping everything, so each case
// also names the tests the cached run must have re-mapped: the narrowing it is
// there to exercise is asserted, not assumed.

// diffBase is the module every case starts from, before its own files.
var diffBase = map[string]string{
	"go.mod":            "module example.com/m\n\ngo 1.22\n",
	"calc/calc.go":      "package calc\n\nfunc Double(n int) int {\n\treturn n * 2\n}\n\nfunc Triple(n int) int {\n\treturn n * 3\n}\n",
	"calc/calc_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\t_ = Double(2)\n}\n\nfunc TestTriple(t *testing.T) {\n\t_ = Triple(2)\n}\n",
}

// diffRemoved, as a file's content, deletes it.
const diffRemoved = "\x00removed"

const diffPkg = "example.com/m/calc"

// diffCase is one edit, and the tests the cached run must map again after it.
type diffCase struct {
	before   map[string]string
	after    map[string]string
	remapped []string
}

// diffModule is one case's module on disk and the commands its runs issued.
type diffModule struct {
	t    *testing.T
	root string
	ran  []string
}

func (m *diffModule) write(fs map[string]string) {
	m.t.Helper()

	for rel, content := range fs {
		path := filepath.Join(m.root, rel)
		if content == diffRemoved {
			if err := os.Remove(path); err != nil {
				m.t.Fatalf("cannot remove %s: %v", rel, err)
			}

			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			m.t.Fatalf("cannot create the directory of %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			m.t.Fatalf("cannot write %s: %v", rel, err)
		}
	}
}

// command runs the real toolchain, recording each test a run executes. The
// coverage directory an instrumented `go test` of this suite sets is dropped:
// the binaries built here write their own profiles.
func (m *diffModule) command(name string, args ...string) *exec.Cmd {
	for i, arg := range args {
		if arg == "-test.run" && i+1 < len(args) {
			m.ran = append(m.ran, strings.TrimSuffix(strings.TrimPrefix(args[i+1], "^"), "$"))
		}
	}
	cmd := exec.Command(name, args...) //nolint:gosec // the toolchain and the binaries it built
	cmd.Dir = m.root
	cmd.Env = append(os.Environ(), "GOCOVERDIR=", "GOWORK=off")

	return cmd
}

// diffMap is what one run left: each test's profile and its dependency keys.
type diffMap struct {
	profiles map[string]Profile
	deps     map[string][]string
}

// build maps the calc package against a cache directory and returns what the
// map holds for it, as the cache file records it after the run.
func (m *diffModule) build(cacheDir string) diffMap {
	m.t.Helper()

	m.ran = nil
	m.t.Chdir(m.root)
	c := NewWithCmd(m.command, m.t.TempDir(),
		gomodule.GoModule{Name: "example.com/m", Root: m.root, CallingDir: "calc"},
		WithTestMapCacheDir(cacheDir))
	tm, err := c.BuildTestMap()
	if err != nil {
		m.t.Fatalf("BuildTestMap() error: %v", err)
	}
	if !tm.Mapped(diffPkg) {
		m.t.Fatalf("%s was not mapped", diffPkg)
	}
	out := diffMap{profiles: map[string]Profile{}}
	for id, p := range tm.profiles {
		if id.Pkg == diffPkg {
			out.profiles[id.Name] = p
		}
	}
	dir, err := c.cacheDirPath(cacheKey(c.cacheScope(), c.buildTags))
	if err != nil {
		m.t.Fatalf("cannot locate the cache: %v", err)
	}
	entry, ok := loadCachedPackage(dir, diffPkg)
	if !ok {
		m.t.Fatalf("no cache entry was written for %s", diffPkg)
	}
	out.deps = entry.Deps
	sort.Strings(m.ran)

	return out
}

func runDifferential(t *testing.T, cases map[string]diffCase) {
	t.Helper()

	if testing.Short() {
		t.Skip("builds and runs real test binaries")
	}
	log.Init(&bytes.Buffer{}, &bytes.Buffer{})
	t.Cleanup(log.Reset)

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := &diffModule{t: t, root: t.TempDir()}
			m.write(diffBase)
			m.write(c.before)
			cache := t.TempDir()
			m.build(cache)

			m.write(c.after)
			reused := m.build(cache)
			ran := m.ran
			scratch := m.build(t.TempDir())

			if diff := cmp.Diff(scratch.profiles, reused.profiles, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("a mapping kept from the cache differs from a fresh one (-fresh +kept):\n%s", diff)
			}
			if diff := cmp.Diff(scratch.deps, reused.deps, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("dependency keys kept from the cache differ from fresh ones (-fresh +kept):\n%s", diff)
			}
			if diff := cmp.Diff(c.remapped, ran, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("re-mapped the wrong tests (-want +got):\n%s", diff)
			}
		})
	}
}

// diffCalc is calc.go with each function's body, and anything before them.
func diffCalc(header, double, triple string) string {
	return "package calc\n" + header + "\nfunc Double(n int) int {\n\t" + double +
		"\n}\n\nfunc Triple(n int) int {\n\t" + triple + "\n}\n"
}

var bothTests = []string{"TestDouble", "TestTriple"}

// The changes the closure narrows: the cached run maps again only the tests a
// change could reach, and what it keeps is what a fresh run produces.
func TestDifferentialNarrowedChanges(t *testing.T) {
	vm := func(clamp string) string {
		return "package vm\n\nfunc Clamp(n int) int {\n\t" + clamp + "\n}\n\nfunc Size(v []int) int {\n\treturn len(v)\n}\n"
	}
	describer := "package calc\n\ntype counter int\n\nfunc describe(v any) int {\n\tif _, ok := v.(interface{ String() string }); ok {\n\t\treturn 1\n\t}\n\treturn 0\n}\n"
	handler := func(twice string) string {
		return "package calc\n\nimport \"reflect\"\n\n" + twice +
			"\n\nfunc register(h any) int {\n\tif reflect.TypeOf(h).NumIn() > 1 {\n\t\treturn 1\n\t}\n\treturn 0\n}\n"
	}
	point := func(tag string) string {
		return "package calc\n\nimport \"reflect\"\n\ntype point struct {\n\tX int `json:\"" + tag +
			"\"`\n}\n\nfunc size(p point) int {\n\tif reflect.TypeOf(p).Field(0).Tag.Get(\"json\") == \"y\" {\n\t\treturn 1\n\t}\n\treturn p.X\n}\n"
	}
	helper := func(body string) string {
		return "package calc\n\nimport \"testing\"\n\nfunc want(t *testing.T, got int) {\n\tt.Helper()\n\t" + body +
			"\n}\n\nfunc TestDouble(t *testing.T) {\n\t_ = Double(2)\n}\n\nfunc TestTriple(t *testing.T) {\n\twant(t, Triple(2))\n}\n"
	}
	runDifferential(t, map[string]diffCase{
		"an added type and function": {
			after: map[string]string{"calc/extra.go": "package calc\n\ntype unit int\n\nfunc Quadruple(n unit) unit {\n\treturn n * 4\n}\n"},
		},
		"a constant one function uses": {
			before: map[string]string{
				"calc/calc.go":   diffCalc("", "if factor > 2 {\n\t\treturn n * factor\n\t}\n\treturn n * 2", "return n * 3"),
				"calc/consts.go": "package calc\n\nconst factor = 2\n",
			},
			after:    map[string]string{"calc/consts.go": "package calc\n\nconst factor = 3\n"},
			remapped: []string{"TestDouble"},
		},
		"a name inserted into a const group under iota": {
			before: map[string]string{
				"calc/calc.go":   diffCalc("", "if b > 1 {\n\t\treturn n\n\t}\n\treturn n * 2", "return n * 3"),
				"calc/consts.go": "package calc\n\nconst (\n\ta = iota\n\tb\n)\n",
			},
			after:    map[string]string{"calc/consts.go": "package calc\n\nconst (\n\tpad = iota\n\ta\n\tb\n)\n"},
			remapped: []string{"TestDouble"},
		},
		"a struct tag read by reflection": {
			before: map[string]string{
				"calc/calc.go":  diffCalc("", "return n*2 + size(point{})", "return n * 3"),
				"calc/types.go": point("x"),
			},
			after:    map[string]string{"calc/types.go": point("y")},
			remapped: []string{"TestDouble"},
		},
		"an added method that satisfies an interface": {
			before: map[string]string{
				"calc/calc.go":  diffCalc("", "return describe(counter(n))", "return n * 3"),
				"calc/types.go": describer,
			},
			after:    map[string]string{"calc/types.go": describer + "\nfunc (c counter) String() string {\n\treturn \"\"\n}\n"},
			remapped: []string{"TestDouble"},
		},
		"a handler's signature, the handler passed as a value": {
			before: map[string]string{
				"calc/calc.go":    diffCalc("", "return register(twice)", "return n * 3"),
				"calc/handler.go": handler("func twice(n int) int {\n\treturn n * 2\n}"),
			},
			after:    map[string]string{"calc/handler.go": handler("func twice(n int, _ int) int {\n\treturn n * 2\n}")},
			remapped: []string{"TestDouble"},
		},
		"a handler's body, the handler passed as a value": {
			before: map[string]string{
				"calc/calc.go":    diffCalc("", "return register(twice)", "return n * 3"),
				"calc/handler.go": handler("func twice(n int) int {\n\treturn n * 2\n}"),
			},
			after: map[string]string{"calc/handler.go": handler("func twice(n int) int {\n\treturn n + n\n}")},
		},
		// Both packages are linked before and after, so only the name moved.
		"an import swapped under the same name": {
			before: map[string]string{
				"vm/vm.go":      vm("return n"),
				"vm2/vm2.go":    "package vm2\n\nfunc Size(v []int) int {\n\treturn 2 * len(v)\n}\n",
				"calc/calc.go":  diffCalc("\nimport st \"example.com/m/vm\"\n", "return st.Size(nil) + n*2", "return n * 3"),
				"calc/other.go": "package calc\n\nimport (\n\t\"example.com/m/vm\"\n\t\"example.com/m/vm2\"\n)\n\nfunc other() int {\n\treturn vm.Size(nil) + vm2.Size(nil)\n}\n",
			},
			after:    map[string]string{"calc/calc.go": diffCalc("\nimport st \"example.com/m/vm2\"\n", "return st.Size(nil) + n*2", "return n * 3")},
			remapped: []string{"TestDouble"},
		},
		"a //go:embed target": {
			before: map[string]string{
				"calc/calc.go": diffCalc("", "if len(data) > 1 {\n\t\treturn n\n\t}\n\treturn n * 2", "return n * 3"),
				"calc/data.go": "package calc\n\nimport _ \"embed\"\n\n//go:embed a.txt\nvar data string\n",
				"calc/a.txt":   "a",
				"calc/b.txt":   "bb",
			},
			after:    map[string]string{"calc/data.go": "package calc\n\nimport _ \"embed\"\n\n//go:embed b.txt\nvar data string\n"},
			remapped: []string{"TestDouble"},
		},
		"a predeclared name shadowed": {
			before:   map[string]string{"calc/calc.go": diffCalc("", "return min(n*2, 100)", "return n * 3")},
			after:    map[string]string{"calc/shadow.go": "package calc\n\nfunc min(a, b int) int {\n\tif a > b {\n\t\treturn b\n\t}\n\treturn a\n}\n"},
			remapped: []string{"TestDouble"},
		},
		"a dependency function's body": {
			before: map[string]string{
				"vm/vm.go":     vm("return n"),
				"calc/calc.go": diffCalc("\nimport \"example.com/m/vm\"\n", "return vm.Clamp(n) * 2", "return n*3 + vm.Size(nil)"),
			},
			after:    map[string]string{"vm/vm.go": vm("if n > 100 {\n\t\treturn 100\n\t}\n\treturn n")},
			remapped: []string{"TestDouble"},
		},
		"a test helper's body": {
			before:   map[string]string{"calc/calc_test.go": helper("_ = got")},
			after:    map[string]string{"calc/calc_test.go": helper("if got < 0 {\n\t\tt.Fail()\n\t}")},
			remapped: []string{"TestTriple"},
		},
		// Every operand is a literal or a stdlib error, so nothing of the
		// package runs while the variables are initialised.
		"error variables built from literals": {
			before: map[string]string{
				"calc/calc.go": diffCalc("", "return n*2 + len(errBad.Error())", "return n * 3"),
				"calc/errs.go": "package calc\n\nimport \"errors\"\n\nvar errBad = errors.New(\"bad\")\n",
			},
			after: map[string]string{"calc/errs.go": "package calc\n\nimport (\n\t\"errors\"\n\t\"fmt\"\n\t\"regexp\"\n)\n\n" +
				"var errBad = errors.New(\"worse\")\n\nvar errNew = fmt.Errorf(\"new %d: %w\", 1, errBad)\n\nvar pattern = regexp.MustCompile(\"x+\")\n"},
			remapped: []string{"TestDouble"},
		},
	})
}

// The holes an adversarial review proved against the closure, each a change
// that reaches a test by a route no name it mentions carries.
func TestDifferentialChangesNoNameCarries(t *testing.T) {
	item := "\ntype item struct{}\n\nfunc (item) String() string {\n\treturn \"item\"\n}\n"
	hashing := diffCalc("\nimport \"crypto\"\n", "if crypto.MD5.Available() {\n\t\treturn 1\n\t}\n\treturn n * 2", "return n * 3")
	runDifferential(t, map[string]diffCase{
		// fmt.Errorf formats its operands at initialisation, calling their
		// String methods.
		"fmt.Errorf formatting a value with a String method": {
			before:   map[string]string{"calc/errs.go": "package calc\n\nimport \"fmt\"\n\nvar errX = fmt.Errorf(\"x\")\n" + item},
			after:    map[string]string{"calc/errs.go": "package calc\n\nimport \"fmt\"\n\nvar errX = fmt.Errorf(\"%v\", item{})\n" + item},
			remapped: bothTests,
		},
		// The method is declared on the alias and belongs to box, which is
		// the name Double mentions.
		"a method declared through an alias": {
			before: map[string]string{
				"calc/calc.go": diffCalc("\nimport \"fmt\"\n", "return n*2 + len(fmt.Sprint(box{}))", "return n * 3"),
				"calc/box.go":  "package calc\n\ntype box struct{}\n\ntype Alias = box\n",
			},
			after:    map[string]string{"calc/box.go": "package calc\n\ntype box struct{}\n\ntype Alias = box\n\nfunc (Alias) String() string {\n\treturn \"alias\"\n}\n"},
			remapped: []string{"TestDouble"},
		},
		// The other direction: the method is declared on box, and Double
		// names only the alias.
		"a method declared on the type an alias names": {
			before: map[string]string{
				"calc/calc.go": diffCalc("\nimport \"fmt\"\n", "return n*2 + len(fmt.Sprint(Alias{}))", "return n * 3"),
				"calc/box.go":  "package calc\n\ntype box struct{}\n\ntype Alias = box\n",
			},
			after:    map[string]string{"calc/box.go": "package calc\n\ntype box struct{}\n\ntype Alias = box\n\nfunc (box) String() string {\n\treturn \"box\"\n}\n"},
			remapped: []string{"TestDouble"},
		},
		// Across packages: the method is declared on vm.Box, and Double
		// names only calc's alias of it.
		"a method declared on another package's type a local alias names": {
			before: map[string]string{
				"vm/vm.go":     "package vm\n\ntype Box struct{}\n",
				"calc/box.go":  "package calc\n\nimport \"example.com/m/vm\"\n\ntype Alias = vm.Box\n",
				"calc/calc.go": diffCalc("\nimport \"fmt\"\n", "return n*2 + len(fmt.Sprint(Alias{}))", "return n * 3"),
			},
			after:    map[string]string{"vm/vm.go": "package vm\n\ntype Box struct{}\n\nfunc (Box) String() string {\n\treturn \"box\"\n}\n"},
			remapped: []string{"TestDouble"},
		},
		// crypto/md5 registers MD5 when it is initialised, and Double asks
		// whether it is registered; a test file linking it changes Double.
		"a test file linking a package with an init": {
			before: map[string]string{"calc/calc.go": hashing},
			after: map[string]string{"calc/calc_test.go": "package calc\n\nimport (\n\t\"crypto/md5\"\n\t\"testing\"\n)\n\n" +
				"func TestDouble(t *testing.T) {\n\t_ = Double(2)\n}\n\nfunc TestTriple(t *testing.T) {\n\t_ = Triple(md5.Size)\n}\n"},
			remapped: bothTests,
		},
		"a dependency linking a package with an init": {
			before: map[string]string{
				"calc/calc.go": diffCalc("\nimport (\n\t\"crypto\"\n\n\t\"example.com/m/vm\"\n)\n",
					"if crypto.MD5.Available() {\n\t\treturn 1\n\t}\n\treturn n * 2", "return vm.Size(nil)"),
				"vm/vm.go": "package vm\n\nfunc Size(v []int) int {\n\treturn len(v)\n}\n",
			},
			after:    map[string]string{"vm/hash.go": "package vm\n\nimport \"crypto/md5\"\n\nfunc Sum() int {\n\treturn md5.Size\n}\n"},
			remapped: bothTests,
		},
		// z registers itself when initialised, calc's init reads the
		// registry, and z is linked before and after: only the order the two
		// are initialised in changes, because calc now imports z.
		"an import that reorders initialisation": {
			before: map[string]string{
				"reg/reg.go":   "package reg\n\nvar Names []string\n",
				"z/z.go":       "package z\n\nimport \"example.com/m/reg\"\n\nfunc init() {\n\treg.Names = append(reg.Names, \"z\")\n}\n\nfunc Name() string {\n\treturn \"z\"\n}\n",
				"calc/init.go": "package calc\n\nimport \"example.com/m/reg\"\n\nvar seen int\n\nfunc init() {\n\tseen = len(reg.Names)\n}\n",
				"calc/calc.go": diffCalc("\n// Name is for the external test.\nfunc Name() string {\n\treturn \"calc\"\n}\n", "if seen > 0 {\n\t\treturn 1\n\t}\n\treturn n * 2", "return n * 3"),
				"calc/x_test.go": "package calc_test\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/calc\"\n\t\"example.com/m/z\"\n)\n\n" +
					"func TestDouble(t *testing.T) {\n\t_ = calc.Double(2)\n}\n\nfunc TestTriple(t *testing.T) {\n\t_ = calc.Triple(2)\n}\n\n" +
					"func TestZ(t *testing.T) {\n\t_ = z.Name() + calc.Name()\n}\n",
				"calc/calc_test.go": diffRemoved,
			},
			after:    map[string]string{"calc/name.go": "package calc\n\nimport \"example.com/m/z\"\n\nfunc name() string {\n\treturn z.Name()\n}\n"},
			remapped: []string{"TestDouble", "TestTriple", "TestZ"},
		},
	})
}

// A test file is entities like any other: adding, removing or renaming one is
// attributed by what it declares, and by what it links.
func TestDifferentialTestFiles(t *testing.T) {
	runDifferential(t, map[string]diffCase{
		"a new test file holding only tests": {
			after:    map[string]string{"calc/more_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestMore(t *testing.T) {\n\t_ = Double(3)\n}\n"},
			remapped: []string{"TestMore"},
		},
		"a new test file linking a package with an init": {
			before: map[string]string{"calc/calc.go": diffCalc("\nimport \"crypto\"\n",
				"if crypto.MD5.Available() {\n\t\treturn 1\n\t}\n\treturn n * 2", "return n * 3")},
			after: map[string]string{"calc/hash_test.go": "package calc\n\nimport (\n\t\"crypto/md5\"\n\t\"testing\"\n)\n\n" +
				"func TestHash(t *testing.T) {\n\t_ = md5.Size\n}\n"},
			remapped: []string{"TestDouble", "TestHash", "TestTriple"},
		},
		"a file renamed to a test file": {
			before: map[string]string{
				"calc/helper.go":    "package calc\n\nfunc helper() int {\n\treturn 2\n}\n",
				"calc/calc_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\t_ = Double(2)\n}\n\nfunc TestTriple(t *testing.T) {\n\t_ = Triple(helper())\n}\n",
			},
			after: map[string]string{
				"calc/helper.go":      diffRemoved,
				"calc/helper_test.go": "package calc\n\nfunc helper() int {\n\treturn 2\n}\n",
			},
			remapped: []string{"TestTriple"},
		},
	})
}
