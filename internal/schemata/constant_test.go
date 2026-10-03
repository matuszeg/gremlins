/*
 * Copyright 2026 The Gremlins Authors
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

package schemata_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-gremlins/gremlins/internal/engine"
	"github.com/go-gremlins/gremlins/internal/schemata"
)

// fixtureImporter resolves the fixture's own packages before the standard
// library.
type fixtureImporter map[string]*types.Package

func (m fixtureImporter) Import(path string) (*types.Package, error) {
	if p, ok := m[path]; ok {
		return p, nil
	}

	return importer.Default().Import(path)
}

// readFixture reads testdata/constant/name.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "constant", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// refusedMarkers maps each line of src carrying "// refused <op>" to op.
func refusedMarkers(t *testing.T, src []byte) map[int]string {
	t.Helper()
	out := map[int]string{}
	sc := bufio.NewScanner(bytes.NewReader(src))
	for line := 1; sc.Scan(); line++ {
		_, after, ok := strings.Cut(sc.Text(), "// refused ")
		if !ok {
			continue
		}
		op, _, _ := strings.Cut(after, ":")
		out[line] = op
	}

	return out
}

// buildApp writes files as the fixture module and compiles the app
// package's test binary.
func buildApp(t *testing.T, files map[string][]byte) string {
	t.Helper()
	files = maps.Clone(files) // writeModule adds go.mod
	dir := writeModule(t, files)
	bin := filepath.Join(dir, "app.test")
	if out, err := goCmd(t, dir, nil, "test", "-c", "-o", bin, "./app"); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	return bin
}

// TestConstantFormsBehave renders every default-mutator site of the app
// package of testdata/constant -- constant-valued sites in every context the
// forms distinguish, with types only foo can name -- and requires the
// refusals to be exactly the marked sites, and the schema binary, run with
// each mutant id (and with none), to print what the plain token mutant
// prints. A refused site keeps its text, so its mutants leave the schema
// printing what the original prints, and never record a reach.
func TestConstantFormsBehave(t *testing.T) {
	t.Parallel()
	fooSrc, src, driver := readFixture(t, "foo/foo.go"), readFixture(t, "app/app.go"), readFixture(t, "app/app_test.go")
	fset := token.NewFileSet()
	fooFile, err := parser.ParseFile(fset, "foo/foo.go", fooSrc, 0)
	if err != nil {
		t.Fatal(err)
	}
	fooPkg, err := (&types.Config{Importer: importer.Default()}).Check("fixture/foo", fset, []*ast.File{fooFile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(fset, "app/app.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	info := newInfo()
	conf := types.Config{Importer: fixtureImporter{"fixture/foo": fooPkg}}
	if _, err := conf.Check("fixture/app", fset, []*ast.File{f}, info); err != nil {
		t.Fatal(err)
	}
	df, err := parser.ParseFile(fset, "app/app_test.go", driver, 0)
	if err != nil {
		t.Fatal(err)
	}

	sites, plain := discover(f)
	prefix := schemata.ChoosePrefix([]*ast.File{f, df})
	h := &schemata.HelperSet{}
	out, errs := schemata.Render(fset, fset.File(f.Pos()), src, sites, schemata.NewRewriter(info, []*ast.File{f}, prefix, h))
	want := refusedMarkers(t, src)
	refused := map[token.Pos]bool{}
	refusedLines := map[int]bool{}
	for _, e := range errs {
		tn, ok := engine.NewTokenNode(e.Site.Node)
		if !ok {
			t.Fatalf("site %T is not a token node", e.Site.Node)
		}
		line := fset.Position(tn.TokPos).Line
		if !errors.Is(e.Err, schemata.ErrUnsupported) || want[line] != e.Site.Tok.String() {
			t.Errorf("line %d: %s site refused: %v", line, e.Site.Tok, e.Err)
		}
		refused[tn.TokPos] = true
		refusedLines[line] = true
	}
	for line, op := range want {
		if !refusedLines[line] {
			t.Errorf("line %d: %s site rewritten, want ErrUnsupported", line, op)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	if got, want := bytes.Count(beforeDuplicates(out), []byte("\n")), bytes.Count(src, []byte("\n")); got != want {
		t.Fatalf("rewrite has %d lines, original %d", got, want)
	}
	helpers, err := h.File("app", prefix)
	if err != nil {
		t.Fatalf("helper file: %v\n%s", err, helpers)
	}
	module := map[string][]byte{"foo/foo.go": fooSrc, "app/app_test.go": driver}
	schemaFiles := maps.Clone(module)
	schemaFiles["app/app.go"] = out
	schemaFiles["app/gremlins_schemata.go"] = helpers
	schemaBin := buildApp(t, schemaFiles)
	origFiles := maps.Clone(module)
	origFiles["app/app.go"] = src
	origBin := buildApp(t, origFiles)
	orig := runFixture(t, origBin)

	t.Run("id0", func(t *testing.T) {
		t.Parallel()
		reach := filepath.Join(t.TempDir(), "reached")
		if got := runFixture(t, schemaBin, "GREMLINS_REACHED="+reach); !bytes.Equal(got, orig) {
			t.Errorf("schema without a mutant printed\n%s\nthe original printed\n%s", got, orig)
		}
		if _, err := os.Stat(reach); err == nil {
			t.Error("reach file written with no mutant active")
		}
	})
	for _, p := range plain {
		name := fmt.Sprintf("id%d_%s_%s_line%d", p.id, p.mt, p.tok, fset.Position(p.pos).Line)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reach := filepath.Join(t.TempDir(), "reached")
			got := runFixture(t, schemaBin, "GREMLINS_MUTANT="+strconv.Itoa(p.id), "GREMLINS_REACHED="+reach)
			_, statErr := os.Stat(reach)
			if refused[p.pos] {
				if !bytes.Equal(got, orig) || statErr == nil {
					t.Errorf("refused site's mutant %d changed the schema (reached: %v):\n%s", p.id, statErr == nil, got)
				}

				return
			}
			to, ok := engine.TokenMutation(p.mt, p.tok)
			if !ok {
				t.Fatalf("engine has no %s mutation for %s", p.mt, p.tok)
			}
			off := fset.Position(p.pos).Offset
			mutated := maps.Clone(module)
			mutated["app/app.go"] = slices.Concat(src[:off:off], []byte(to.String()), src[off+len(p.tok.String()):])
			want := runFixture(t, buildApp(t, mutated))
			if bytes.Equal(want, orig) {
				t.Errorf("plain mutant %d prints what the original prints: the fixture does not observe it", p.id)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("schema with mutant %d printed\n%s\nthe plain mutant printed\n%s", p.id, got, want)
			}
			if statErr != nil {
				t.Errorf("reach file missing: %v", statErr)
			}
		})
	}
}

// TestConstMutationsMatchEngine holds the folding table of the constant
// forms to the engine's: for every default mutator and every token, both
// rewrite it to the same token, or neither applies. ++ and -- are
// statements, never constant, and have no folding entry.
func TestConstMutationsMatchEngine(t *testing.T) {
	t.Parallel()
	for _, mt := range formsMutators {
		for tok := token.ILLEGAL; tok <= token.TILDE; tok++ {
			if tok == token.INC || tok == token.DEC {
				continue
			}
			want, wok := engine.TokenMutation(mt, tok)
			got, gok := schemata.ConstMutation(mt, tok)
			if got != want || gok != wok {
				t.Errorf("%s %s: folding rewrites to %s (%v), engine to %s (%v)", mt, tok, got, gok, want, wok)
			}
		}
	}
}
