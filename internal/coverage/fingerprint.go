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
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// fingerprint is what a package's source looked like when its map was made.
//
// It exists because the build ID is too coarse to invalidate a map with. Every
// test of a package shares one test binary, so one changed line moves the build
// ID and dirties all of the package's mappings — and the package under mutation
// is by definition the package that was changed, so that is the case that
// always happens rather than the rare one.
//
// The fingerprint splits the package into the two things a diff can treat
// differently: the declarations whose change can be attributed to particular
// mappings, and everything else — imports, constants, package-level variables,
// types, struct tags, test helpers, non-Go files — whose change cannot, and so
// dirties the whole package.
type fingerprint struct {
	// Decls is every declaration whose change can be attributed, with the span
	// it occupied when the map was made. The span is in that run's coordinates
	// on purpose: the profiles it will be compared against are too.
	Decls map[string]declPrint `json:"decls"`

	// Shell is one hash over everything else, across every file of the
	// package's directory. A change to any of it dirties the whole package: a
	// `const timeout = 5` becoming `10` changes a line no coverage block
	// contains, while the tests that execute the use site do change behaviour.
	//
	// It is built so that adding, removing or moving a function does not move
	// it — declarations are cut out rather than blanked, and the whitespace
	// they leave behind is dropped.
	Shell string `json:"shell"`
}

// The kinds of declaration whose effect reaches past the lines it occupies.
const (
	// kindInit is an init function. It runs before every test in the binary, so
	// any change to one dirties the whole package rather than a line range.
	kindInit = "init"
	// kindMethod is a method. Changing its body reaches only its own lines, but
	// adding or removing one changes which interfaces the receiver satisfies,
	// which can redirect a type switch in code that did not itself change.
	kindMethod = "method"
)

// declPrint is one declaration: what it said, and where it was.
type declPrint struct {
	Hash string `json:"hash"`
	// File is the name the coverage profile uses, not the name on disk, so that
	// a span can be compared against a profile without translating either.
	File string `json:"file"`
	// Test is the name `go test` runs this declaration under, when it is a test
	// rather than package code. Coverage does not instrument test files, so a
	// test's own lines appear in no profile and a change to one is attributed
	// by name instead of by span.
	Test string `json:"test,omitempty"`
	Kind string `json:"kind,omitempty"`

	Start int `json:"start"`
	End   int `json:"end"`
}

// testFuncPrefixes are the declarations `go test` runs on their own, and so the
// only ones in a test file whose change can be attributed to a single mapping.
// They match listPattern, which decides what goes into the map in the first
// place.
var testFuncPrefixes = []string{"Test", "Fuzz", "Example"}

// goFile is a package file as the fingerprint reads it: its bytes, and its
// syntax when it has any.
type goFile struct {
	fset *token.FileSet
	ast  *ast.File
	name string
	data []byte
}

// candidate is a declaration that could be attributed, held until the whole
// package has been read: whether it can be depends on what else the package
// declares.
type candidate struct {
	fn   *ast.FuncDecl
	key  string
	decl declPrint
	file int
}

// fingerprintOf reads a package's directory and records its shape.
//
// The whole directory is read rather than the file list `go list` reports, so
// that nothing a change could hide in is left out of the shell: a C file, an
// embedded asset, a file excluded by a build tag. Anything unreadable is a
// failure to fingerprint, and anything unparseable is folded into the shell
// whole — both cost a re-map of the package and neither can give a wrong
// answer.
func (c *Coverage) fingerprintOf(pkg *testPackage) (fingerprint, bool) {
	files, ok := readPackageFiles(pkg.dir)
	if !ok {
		return fingerprint{}, false
	}

	// Both of these are package-wide, not per file. A test another declaration
	// calls does not run only on its own, wherever the caller is; and a key two
	// declarations share cannot tell them apart, wherever the other one is.
	referenced := map[string]bool{}
	for i := range files {
		if files[i].ast != nil {
			collectReferencedNames(files[i].ast, referenced)
		}
	}
	candidates, keyCount := c.candidatesOf(pkg, files, referenced)

	fp := fingerprint{Decls: map[string]declPrint{}}
	cuts := make([][]candidate, len(files))
	for _, cand := range candidates {
		if keyCount[cand.key] != 1 {
			continue
		}
		fp.Decls[cand.key] = cand.decl
		cuts[cand.file] = append(cuts[cand.file], cand)
	}

	shell := make([]string, 0, len(files))
	for i := range files {
		shell = append(shell, files[i].name+"\x00"+hashOf(shellOf(&files[i], cuts[i])))
	}
	fp.Shell = hashOf([]byte(strings.Join(shell, "\x00")))

	return fp, true
}

// readPackageFiles reads every regular file of a package directory, parsing the
// Go ones. A file that does not parse keeps a nil syntax tree, which is what
// puts the whole of it into the shell.
func readPackageFiles(dir string) ([]goFile, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	files := make([]goFile, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: the directory is one `go list` reported
		if err != nil {
			return nil, false
		}
		f := goFile{name: name, data: data}
		if strings.HasSuffix(name, ".go") {
			f.fset = token.NewFileSet()
			if parsed, perr := parser.ParseFile(f.fset, name, data, parser.ParseComments); perr == nil {
				f.ast = parsed
			}
		}
		files = append(files, f)
	}

	return files, true
}

// candidatesOf describes every declaration whose change could be attributed,
// and counts how many declarations claim each key. A key two declarations share
// belongs to neither: Go allows several init functions in one file, and a test
// name can repeat across a package and its external test package.
func (c *Coverage) candidatesOf(pkg *testPackage, files []goFile,
	referenced map[string]bool,
) ([]candidate, map[string]int) {
	var candidates []candidate
	keyCount := map[string]int{}

	for i := range files {
		f := &files[i]
		if f.ast == nil {
			continue
		}
		profileName := c.profileFileName(pkg.importPath, f.name)
		isTestFile := strings.HasSuffix(f.name, "_test.go")
		for _, decl := range f.ast.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			described, key, ok := printOf(f, fn, profileName, isTestFile, referenced)
			if !ok {
				continue
			}
			candidates = append(candidates, candidate{fn: fn, key: key, decl: described, file: i})
			keyCount[key]++
		}
	}

	return candidates, keyCount
}

// printOf describes one declaration, reporting whether its change can be
// attributed at all.
//
// In package code it always can: the profile records which tests executed its
// lines. In a test file only a declaration `go test` runs on its own can be — a
// test-file helper is executed by tests the profile does not record, because
// coverage does not instrument test files — so everything else in a test file
// stays in the shell.
func printOf(f *goFile, fn *ast.FuncDecl, profileName string, isTestFile bool,
	referenced map[string]bool,
) (declPrint, string, bool) {
	start, end := declSpan(f.fset, fn)
	described := declPrint{
		Hash:  hashOf(f.data[start.Offset:end.Offset]),
		File:  profileName,
		Start: start.Line,
		End:   end.Line,
	}
	switch {
	case fn.Recv != nil:
		described.Kind = kindMethod
	case fn.Name.Name == "init":
		described.Kind = kindInit
	}

	if !isTestFile {
		return described, profileName + ":" + declKey(fn), true
	}
	if fn.Recv != nil || !isTestFuncName(fn.Name.Name) || referenced[fn.Name.Name] {
		return declPrint{}, "", false
	}
	described.Test = fn.Name.Name

	// Keyed by test name alone, so that moving a test between test files does
	// not read as one test removed and another added.
	return described, "test:" + fn.Name.Name, true
}

// shellOf is what is left of a file once its attributable declarations are cut
// out.
//
// They are removed rather than blanked, and whitespace-only remainders are
// dropped, so that adding, removing or moving a function leaves the shell
// exactly where it was. That is what makes "a new free function changes
// nothing" true of the fingerprint as well as of the program.
func shellOf(f *goFile, cuts []candidate) []byte {
	if len(cuts) == 0 {
		return f.data
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].fn.Pos() < cuts[j].fn.Pos() })

	var kept []string
	cut := 0
	for _, cand := range cuts {
		start, end := declSpan(f.fset, cand.fn)
		if gap := strings.TrimSpace(string(f.data[cut:start.Offset])); gap != "" {
			kept = append(kept, gap)
		}
		cut = end.Offset
	}
	if gap := strings.TrimSpace(string(f.data[cut:])); gap != "" {
		kept = append(kept, gap)
	}

	return []byte(strings.Join(kept, "\x00"))
}

// declSpan is the whole of a declaration as written, including its doc comment.
//
// The comment is inside the span because a directive lives there: //go:noinline
// and //go:linkname change what the code does, and telling those from prose is
// not worth the risk of getting it wrong.
func declSpan(fset *token.FileSet, fn *ast.FuncDecl) (token.Position, token.Position) {
	pos := fn.Pos()
	if fn.Doc != nil {
		pos = fn.Doc.Pos()
	}

	return fset.Position(pos), fset.Position(fn.End())
}

// declKey names a declaration within its file: methods of different types share
// a name, and the receiver is what tells them apart.
func declKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	var recv strings.Builder
	ast.Inspect(fn.Recv.List[0].Type, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			recv.WriteString(id.Name)
		}

		return true
	})

	return recv.String() + "." + fn.Name.Name
}

func isTestFuncName(name string) bool {
	for _, prefix := range testFuncPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}

	return false
}

// collectReferencedNames records every identifier the file uses other than a
// declaration's own name, so that a test something else names is not mistaken
// for one that only ever runs on its own.
func collectReferencedNames(file *ast.File, into map[string]bool) {
	for _, decl := range file.Decls {
		var own *ast.Ident
		if fn, ok := decl.(*ast.FuncDecl); ok {
			own = fn.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if id, isID := n.(*ast.Ident); isID && id != own {
				into[id.Name] = true
			}

			return true
		})
	}
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// profileFileName is the name a coverage profile gives one of a package's
// files. It mirrors removeModuleFromPath, which does the same translation from
// the other direction.
func (c *Coverage) profileFileName(importPath, base string) string {
	path := strings.ReplaceAll(importPath+"/"+base, c.mod.Name+"/", "")
	rel, err := filepath.Rel(c.mod.CallingDir, path)
	if err != nil {
		return path
	}

	return rel
}
