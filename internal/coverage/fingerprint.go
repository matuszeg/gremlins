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
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
// It is the package's own print (see pkgPrint) — every entity of the package
// and its test files, and the remainder hashed whole — plus one print per
// instrumented dependency, and Inputs for everything else the binary is built
// from. reusable reads two of them to decide which mappings a change reached.
type fingerprint struct {
	pkgPrint

	// Inputs is everything the test binary is built from except this package
	// and the dependencies in Deps: see buildInputsOf, which fills it in.
	// Without it a moved build ID could never be told apart from a moved
	// dependency, and narrowing would keep stale mappings. With it, a moved
	// build ID under an unchanged fingerprint (Deps included) and unchanged
	// Inputs is a moved checkout, and the whole map is kept.
	//
	// It is not read off the filesystem like the rest, so fingerprintOf leaves
	// it empty and the caller sets it — which is also what makes a fingerprint
	// with no inputs unusable rather than optimistic.
	Inputs string `json:"inputs"`

	// Deps is every dependency the test binary instruments besides this
	// package, by import path: the packages of every main module it links (see
	// dependencyDirs). Their lines are recorded per test, as the entities it
	// executed there, so a dependency is printed like the package rather than
	// hashed into Inputs — which is what lets a change in one dirty only the
	// tests it can reach. A dependency's test files are not part of its print:
	// they are never linked into this package's test binary.
	Deps map[string]pkgPrint `json:"deps,omitempty"`
}

// goFile is a package file as the fingerprint reads it: its bytes, and its
// syntax when it has any.
type goFile struct {
	fset *token.FileSet
	ast  *ast.File
	name string
	data []byte
}

// fingerprintOf reads a package's directory and records its shape.
//
// The whole directory is read rather than the file list `go list` reports, so
// that nothing a change could hide in is left out: a C file, an embedded
// asset, a file excluded by a build tag. Anything unreadable is a failure to
// fingerprint, and anything unparseable is folded into Whole — both cost a
// re-map of the package and neither can give a wrong answer.
func (c *Coverage) fingerprintOf(pkg *testPackage) (fingerprint, bool) {
	files, ok := readPackageFiles(pkg.dir)
	if !ok {
		return fingerprint{}, false
	}
	below, ok := hashPackageData(pkg.dir, files)
	if !ok {
		return fingerprint{}, false
	}

	printed := c.printPackage(pkg.importPath, files, true, below, c.typedFacts(pkg.importPath, true))

	return fingerprint{pkgPrint: printed}, true
}

// hashPackageData hashes what a package reads from below its own directory
// without compiling it: its testdata tree, and every file or directory its
// //go:embed directives name.
//
// They belong in Whole because a test can behave differently on new input
// without a line of the package changing, which would leave a kept mapping
// describing a path the test no longer takes. The build ID does see embedded
// files, so on its own that case ends in a whole re-map — but a run that has
// already found a reason to narrow would never get that far.
//
// It is those and nothing else. Hashing every directory beside the sources
// instead made a change to data the package never reads — sqlc's query files
// beside a package that embeds only its migrations — re-map every package
// that links it. And it still missed what the package does read when that
// sits in a directory holding Go files: migrations/*.sql beside a migrations
// package. A test that opens a sibling directory at run time is unseen either
// way, as anything above the directory always was.
//
// The patterns are read from every Go file of the directory, whichever a
// build would select, and matched by the toolchain's own glob rules. That
// reads more than the build embeds — a dotfile below a plain directory
// pattern, a file a build tag leaves out — which costs a re-map and never a
// wrong answer. A directive that does not parse, or a pattern that is not a
// glob, is a failure to fingerprint.
func hashPackageData(dir string, files []goFile) (string, bool) {
	var parts []string
	testdata := filepath.Join(dir, "testdata")
	switch info, err := os.Stat(testdata); {
	case err == nil && info.IsDir():
		sum, ok := hashTree(testdata)
		if !ok {
			return "", false
		}
		parts = append(parts, "testdata\x00"+sum)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", false
	}

	embedded, ok := embeddedPaths(dir, files)
	if !ok {
		return "", false
	}
	for _, rel := range embedded {
		sum, pathOK := hashPath(filepath.Join(dir, filepath.FromSlash(rel)))
		if !pathOK {
			return "", false
		}
		parts = append(parts, "embed\x00"+rel+"\x00"+sum)
	}

	return hashOf([]byte(strings.Join(parts, "\x00"))), true
}

// embeddedPaths is what below dir a //go:embed pattern of one of files
// reaches, slash-separated and sorted.
//
// A top-level file a pattern names is left out: it is a file of the directory,
// which the fingerprint hashes already. A top-level directory holding no Go
// file stands whole for every match inside it, so a directive retargeted from
// one of its files to another moves only the directive's own entity, as it
// does for files of the directory itself. A directory that is a package of its
// own contributes only what was matched in it.
func embeddedPaths(dir string, files []goFile) ([]string, bool) {
	matches, ok := embedMatches(dir, files)
	if !ok {
		return nil, false
	}
	reached := map[string]bool{}
	isPkg := map[string]bool{}
	for _, m := range matches {
		top, below, nested := strings.Cut(m, "/")
		if !nested {
			info, err := os.Stat(filepath.Join(dir, top))
			if err != nil {
				return nil, false
			}
			if !info.IsDir() {
				continue
			}
		}
		pkg, seen := isPkg[top]
		if !seen {
			var listOK bool
			pkg, listOK = holdsGoFiles(filepath.Join(dir, top))
			if !listOK {
				return nil, false
			}
			isPkg[top] = pkg
		}
		if pkg && below != "" {
			reached[m] = true
		} else {
			reached[top] = true
		}
	}

	return sortedKeys(reached), true
}

// embedMatches is every path below dir that a //go:embed pattern of one of
// files matches.
func embedMatches(dir string, files []goFile) ([]string, bool) {
	var out []string
	root := os.DirFS(dir)
	for _, f := range files {
		if !strings.HasSuffix(f.name, ".go") {
			continue
		}
		for _, line := range strings.Split(string(f.data), "\n") {
			args, isEmbed := embedDirective(line)
			if !isEmbed {
				continue
			}
			patterns, ok := embedPatterns(args)
			if !ok {
				return nil, false
			}
			for _, pattern := range patterns {
				matches, err := fs.Glob(root, strings.TrimPrefix(pattern, "all:"))
				if err != nil {
					return nil, false
				}
				out = append(out, matches...)
			}
		}
	}

	return out, true
}

func holdsGoFiles(dir string) (bool, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, false
	}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".go") {
			return true, true
		}
	}

	return false, true
}

// embedDirective reports whether line is a //go:embed directive, and what
// follows the directive's name.
func embedDirective(line string) (string, bool) {
	const directive = "//go:embed"
	rest, found := strings.CutPrefix(strings.TrimLeft(line, " \t"), directive)
	if !found || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}

	return strings.TrimRight(rest, "\r"), true
}

// embedPatterns splits a //go:embed directive's arguments the way the
// toolchain does: on spaces, with a Go string literal, interpreted or raw,
// standing for one pattern that may hold them.
func embedPatterns(args string) ([]string, bool) {
	var out []string
	for {
		args = strings.TrimLeft(args, " \t")
		if args == "" {
			return out, true
		}
		var pattern string
		switch args[0] {
		case '"':
			end := 1
			for end < len(args) && args[end] != '"' {
				if args[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(args) {
				return nil, false
			}
			unquoted, err := strconv.Unquote(args[:end+1])
			if err != nil {
				return nil, false
			}
			pattern, args = unquoted, args[end+1:]
		case '`':
			end := strings.IndexByte(args[1:], '`')
			if end < 0 {
				return nil, false
			}
			pattern, args = args[1:1+end], args[2+end:]
		default:
			end := strings.IndexAny(args, " \t")
			if end < 0 {
				end = len(args)
			}
			pattern, args = args[:end], args[end:]
		}
		out = append(out, pattern)
	}
}

// hashPath hashes one embedded path: a file's content, or a directory's whole
// tree.
func hashPath(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return hashTree(path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: a path a //go:embed pattern of the package matched
	if err != nil {
		return "", false
	}

	return hashOf(data), true
}

// hashTree hashes every regular file below dir, by path and content.
func hashTree(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			sum, ok := hashTree(path)
			if !ok {
				return "", false
			}
			parts = append(parts, "dir\x00"+e.Name()+"\x00"+sum)
		case e.Type().IsRegular():
			data, err := os.ReadFile(path) //nolint:gosec // G304: below a directory `go list` reported
			if err != nil {
				return "", false
			}
			parts = append(parts, e.Name()+"\x00"+hashOf(data))
		}
	}

	return hashOf([]byte(strings.Join(parts, "\x00"))), true
}

// readPackageFiles reads every regular file of a package directory, parsing the
// Go ones. A file that does not parse keeps a nil syntax tree, which is what
// puts the whole of it into Whole.
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

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// profileFileName is the name a coverage profile gives one of a package's
// files. It mirrors removeModuleFromPath, which does the same translation from
// the other direction, and is module-relative for the same reason.
func (c *Coverage) profileFileName(importPath, base string) string {
	return strings.ReplaceAll(importPath+"/"+base, c.mod.Name+"/", "")
}
