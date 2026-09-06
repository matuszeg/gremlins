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
	"testing"

	"github.com/go-gremlins/gremlins/internal/gomodule"
)

// fingerprintOfSource writes the given files into a package directory and
// fingerprints it, the way a run does before deciding what its cache still
// says.
func fingerprintOfSource(t *testing.T, files map[string]string) fingerprint {
	t.Helper()

	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("cannot write the source: %v", err)
		}
	}
	c := &Coverage{mod: gomodule.GoModule{Name: "example.com", Root: dir, CallingDir: "."}}

	fp, ok := c.fingerprintOf(&testPackage{importPath: "example.com/pkg", dir: dir})
	if !ok {
		t.Fatal("want the package fingerprinted, got a failure")
	}

	return fp
}

func TestFingerprintOfFailsOnADirectoryItCannotRead(t *testing.T) {
	t.Parallel()

	c := &Coverage{mod: gomodule.GoModule{Name: "example.com", Root: ".", CallingDir: "."}}
	pkg := &testPackage{importPath: "example.com/gone", dir: filepath.Join(t.TempDir(), "gone")}

	// Failing to read is not failing the run: the caller re-maps the package,
	// which is what happens without a cache at all.
	if _, ok := c.fingerprintOf(pkg); ok {
		t.Error("want a failure for a directory that is not there")
	}
}

// The shell has to be blind to a function being added, moved or removed, or
// every such edit would re-map the whole package through the back door.
func TestFingerprintShellIgnoresWhereTheFunctionsAre(t *testing.T) {
	t.Parallel()

	base := fingerprintOfSource(t, map[string]string{"a.go": `package pkg

const factor = 2

func F() int {
	return factor
}

func G() int {
	return 3
}
`})

	testCases := map[string]string{
		"a function added": `package pkg

const factor = 2

func F() int {
	return factor
}

func G() int {
	return 3
}

func H() int {
	return 4
}
`,
		"the functions reordered": `package pkg

const factor = 2

func G() int {
	return 3
}

func F() int {
	return factor
}
`,
		"a function's body changed": `package pkg

const factor = 2

func F() int {
	return factor * 2
}

func G() int {
	return 3
}
`,
	}
	for name, source := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := fingerprintOfSource(t, map[string]string{"a.go": source}); got.Shell != base.Shell {
				t.Error("want the shell unmoved by a change confined to the functions")
			}
		})
	}

	t.Run("the constant changed", func(t *testing.T) {
		t.Parallel()

		moved := fingerprintOfSource(t, map[string]string{"a.go": `package pkg

const factor = 3

func F() int {
	return factor
}

func G() int {
	return 3
}
`})
		if moved.Shell == base.Shell {
			t.Error("want a change outside every function to move the shell")
		}
	})
}

// A non-Go file is in the shell whole, so an embedded asset or a C source
// changing dirties the package rather than passing unnoticed.
func TestFingerprintCoversEveryFileOfTheDirectory(t *testing.T) {
	t.Parallel()

	base := fingerprintOfSource(t, map[string]string{
		"a.go":     "package pkg\n\nfunc F() int {\n\treturn 1\n}\n",
		"data.txt": "one",
	})
	changed := fingerprintOfSource(t, map[string]string{
		"a.go":     "package pkg\n\nfunc F() int {\n\treturn 1\n}\n",
		"data.txt": "two",
	})

	if base.Shell == changed.Shell {
		t.Error("want a change to a non-Go file to move the shell")
	}
}

// A file that does not parse yields no declarations, so all of it is in the
// shell and any change to it dirties the package.
func TestFingerprintPutsAnUnparseableFileInTheShell(t *testing.T) {
	t.Parallel()

	broken := fingerprintOfSource(t, map[string]string{"a.go": "package pkg\n\nfunc F( {\n"})
	if len(broken.Decls) != 0 {
		t.Errorf("want no declarations from a file that does not parse, got %d", len(broken.Decls))
	}

	other := fingerprintOfSource(t, map[string]string{"a.go": "package pkg\n\nfunc G( {\n"})
	if broken.Shell == other.Shell {
		t.Error("want a change to an unparseable file to move the shell")
	}
}

func TestFingerprintDescribesTheDeclarationsItCanAttribute(t *testing.T) {
	t.Parallel()

	fp := fingerprintOfSource(t, map[string]string{
		"a.go": `package pkg

// F doubles n.
func F(n int) int {
	return n * 2
}

func (t T) M() int {
	return 1
}

func init() {
	_ = F(1)
}
`,
		"a_test.go": `package pkg

import "testing"

func helper(t *testing.T) int {
	return F(1)
}

func TestF(t *testing.T) {
	_ = helper(t)
}

func TestCalled(t *testing.T) {
	_ = 1
}

func TestCaller(t *testing.T) {
	TestCalled(t)
}
`,
	})

	// The doc comment is inside the span, because a //go: directive lives there
	// and telling one from prose is not worth getting wrong.
	f, ok := fp.Decls["pkg/a.go:F"]
	if !ok {
		t.Fatalf("want F described, got keys %v", keysOf(fp.Decls))
	}
	if f.Start != 3 || f.End != 6 || f.Kind != "" || f.File != "pkg/a.go" {
		t.Errorf("want F over lines 3-6 of pkg/a.go as plain code, got %+v", f)
	}
	if m := fp.Decls["pkg/a.go:T.M"]; m.Kind != kindMethod {
		t.Errorf("want the method marked as one, got %+v", m)
	}
	if i := fp.Decls["pkg/a.go:init"]; i.Kind != kindInit {
		t.Errorf("want init marked as one, got %+v", i)
	}

	// A test file's helper is executed by tests the profile does not record, so
	// it cannot be attributed and stays in the shell.
	if _, found := fp.Decls["pkg/a_test.go:helper"]; found {
		t.Error("want a test-file helper left in the shell")
	}
	if got := fp.Decls["test:TestF"]; got.Test != "TestF" {
		t.Errorf("want TestF attributable by name, got %+v", got)
	}
	// A test another declaration names is not run only on its own either.
	if _, found := fp.Decls["test:TestCalled"]; found {
		t.Error("want a test that something else calls left in the shell")
	}
	if got := fp.Decls["test:TestCaller"]; got.Test != "TestCaller" {
		t.Errorf("want the calling test still attributable, got %+v", got)
	}
}

// Two declarations under one key cannot be told apart, so neither is narrowed.
// Go allows several init functions in one file, and a test name can repeat
// across a package and its external test package.
func TestFingerprintLeavesAmbiguousDeclarationsInTheShell(t *testing.T) {
	t.Parallel()

	files := map[string]string{
		"a.go": `package pkg

func init() {
	_ = 1
}

func init() {
	_ = 2
}
`,
		"a_test.go": `package pkg

import "testing"

func TestF(t *testing.T) {
	_ = 1
}
`,
		"b_test.go": `package pkg_test

import "testing"

func TestF(t *testing.T) {
	_ = 2
}
`,
	}
	fp := fingerprintOfSource(t, files)

	if _, found := fp.Decls["pkg/a.go:init"]; found {
		t.Error("want two inits of one file left in the shell")
	}
	if _, found := fp.Decls["test:TestF"]; found {
		t.Error("want a test name declared twice left in the shell")
	}

	// Left in the shell means a change to one of them still dirties the package.
	files["a.go"] = `package pkg

func init() {
	_ = 1
}

func init() {
	_ = 3
}
`
	if changed := fingerprintOfSource(t, files); changed.Shell == fp.Shell {
		t.Error("want a change to an ambiguous declaration to move the shell")
	}
}

func keysOf(decls map[string]declPrint) []string {
	var keys []string
	for key := range decls {
		keys = append(keys, key)
	}

	return keys
}
