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
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"

	"github.com/go-gremlins/gremlins/internal/mutator"
	"github.com/go-gremlins/gremlins/internal/schemata"
)

// loadPkg loads the package pattern of the fixture module dir the way the
// engine will hand it to RewritePackage.
func loadPkg(t *testing.T, dir, pattern string) *packages.Package {
	t.Helper()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedSyntax,
		Dir: abs,
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || len(pkgs[0].Errors) > 0 {
		t.Fatalf("load %s in %s: %d packages, errors %v", pattern, dir, len(pkgs), pkgs[0].Errors)
	}

	return pkgs[0]
}

// pkgSites discovers the default-mutator sites of every file of pkg and
// numbers their mutants from 1 across the package.
func pkgSites(pkg *packages.Package) []schemata.Site {
	var all []schemata.Site
	next := 0
	for _, f := range pkg.Syntax {
		sites, plain := discover(f)
		for _, s := range sites {
			for i := range s.Muts {
				s.Muts[i].ID += next
			}
			all = append(all, s)
		}
		next += len(plain)
	}

	return all
}

// breakTok returns a rewriter factory that wraps NewRewriter but rewrites
// every site whose token is tok into a call to an undefined helper, which
// does not type-check.
func breakTok(tok token.Token) schemata.RewriterFactory {
	return func(info *types.Info, files []*ast.File, prefix string, h *schemata.HelperSet) schemata.Rewriter {
		rw := schemata.NewRewriter(info, files, prefix, h)

		return func(s schemata.Site, inner func(ast.Node) string) (string, error) {
			if e, ok := s.Node.(*ast.BinaryExpr); ok && s.Tok == tok {
				return prefix + "Nope(" + inner(e.X) + ", " + inner(e.Y) + ")", nil
			}

			return rw(s, inner)
		}
	}
}

// refuseTok returns a rewriter factory that wraps NewRewriter but refuses
// every site whose token is tok.
func refuseTok(tok token.Token) schemata.RewriterFactory {
	return func(info *types.Info, files []*ast.File, prefix string, h *schemata.HelperSet) schemata.Rewriter {
		rw := schemata.NewRewriter(info, files, prefix, h)

		return func(s schemata.Site, inner func(ast.Node) string) (string, error) {
			if s.Tok == tok {
				return "", schemata.ErrUnsupported
			}

			return rw(s, inner)
		}
	}
}

// brokenHelper returns a rewriter factory that wraps NewRewriter but adds a
// helper declaration that does not type-check: an error in the helper file,
// inside no site.
func brokenHelper(info *types.Info, files []*ast.File, prefix string, h *schemata.HelperSet) schemata.Rewriter {
	rw := schemata.NewRewriter(info, files, prefix, h)

	return func(s schemata.Site, inner func(ast.Node) string) (string, error) {
		h.AddRaw("var " + prefix + "Broken int = \"not an int\"")

		return rw(s, inner)
	}
}

// siteKey identifies a site by its node, for accounting.
func siteKey(fset *token.FileSet, s schemata.Site) string {
	return fset.Position(s.Node.Pos()).String() + " " + s.Tok.String()
}

// TestRewritePackage checks the type-check gate: which sites are placed,
// which are dropped and why, that placed and dropped together are exactly the
// input sites, and that the files returned type-check.
func TestRewritePackage(t *testing.T) {
	t.Parallel()
	type want struct {
		placed  []token.Token // tokens of the placed sites, in input order
		reason  string        // substring every dropped site's error holds
		dropped []token.Token
		helper  string // base name of the helper file
	}
	testCases := map[string]struct {
		dir, pattern string
		factory      schemata.RewriterFactory
		want         want
	}{
		"all_placed": {
			dir: "testdata/twopkgs", pattern: "./ok",
			want: want{placed: []token.Token{token.ADD}, helper: "zz__gremlins_schema.go"},
		},
		"prefix_taken_by_a_test_file": {
			dir: "testdata/prefixclash", pattern: ".",
			want: want{placed: []token.Token{token.ADD, token.LSS}, helper: "zz__gremlins2_schema.go"},
		},
		"nested_site_that_does_not_type_check_is_dropped_alone": {
			dir: "testdata/twopkgs", pattern: "./bad", factory: breakTok(token.MUL),
			want: want{
				placed: []token.Token{token.ADD, token.LSS}, helper: "zz__gremlins_schema.go",
				dropped: []token.Token{token.MUL}, reason: "undefined: _gremlinsNope",
			},
		},
		"outer_site_that_does_not_type_check_is_dropped_alone": {
			dir: "testdata/twopkgs", pattern: "./bad", factory: breakTok(token.ADD),
			want: want{
				placed: []token.Token{token.MUL, token.LSS}, helper: "zz__gremlins_schema.go",
				dropped: []token.Token{token.ADD}, reason: "undefined: _gremlinsNope",
			},
		},
		"refused_site_is_dropped_with_its_error": {
			dir: "testdata/twopkgs", pattern: "./bad", factory: refuseTok(token.LSS),
			want: want{
				placed: []token.Token{token.ADD, token.MUL}, helper: "zz__gremlins_schema.go",
				dropped: []token.Token{token.LSS}, reason: schemata.ErrUnsupported.Error(),
			},
		},
		"unattributable_error_drops_package": {
			dir: "testdata/twopkgs", pattern: "./bad", factory: brokenHelper,
			want: want{
				dropped: []token.Token{token.ADD, token.MUL, token.LSS},
				reason:  "unattributable type error: ",
			},
		},
		"module_go_version_below_1_21": {
			dir: "testdata/oldgo", pattern: ".",
			want: want{dropped: []token.Token{token.ADD}, reason: "module go version < 1.21"},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pkg := loadPkg(t, tc.dir, tc.pattern)
			sites := pkgSites(pkg)
			factory := tc.factory
			if factory == nil {
				factory = schemata.NewRewriter
			}
			files, placed, dropped := schemata.RewritePackageWith(pkg, sites, "", factory)

			// placed + dropped is exactly the input, each site once.
			var got []string
			for _, s := range placed {
				got = append(got, siteKey(pkg.Fset, s))
			}
			for _, d := range dropped {
				got = append(got, siteKey(pkg.Fset, d.Site))
			}
			var in []string
			for _, s := range sites {
				in = append(in, siteKey(pkg.Fset, s))
			}
			slices.Sort(got)
			slices.Sort(in)
			if !slices.Equal(got, in) {
				t.Fatalf("placed+dropped = %v, input sites = %v", got, in)
			}

			var placedToks, droppedToks []token.Token
			for _, s := range placed {
				placedToks = append(placedToks, s.Tok)
			}
			for _, d := range dropped {
				droppedToks = append(droppedToks, d.Site.Tok)
				if d.Err == nil || !strings.Contains(d.Err.Error(), tc.want.reason) {
					t.Errorf("site %s dropped with %v, want reason containing %q", siteKey(pkg.Fset, d.Site), d.Err, tc.want.reason)
				}
			}
			slices.Sort(droppedToks)
			wantDropped := slices.Clone(tc.want.dropped)
			slices.Sort(wantDropped)
			if !slices.Equal(placedToks, tc.want.placed) || !slices.Equal(droppedToks, wantDropped) {
				t.Errorf("placed %v dropped %v, want placed %v dropped %v", placedToks, droppedToks, tc.want.placed, wantDropped)
			}

			if len(tc.want.placed) == 0 {
				if len(files) != 0 {
					t.Errorf("no site placed but %d files returned", len(files))
				}

				return
			}
			var helper string
			for path := range files {
				if !filepath.IsAbs(path) || filepath.Dir(path) != pkg.Dir {
					t.Errorf("file %s is not an absolute path in %s", path, pkg.Dir)
				}
				if strings.HasPrefix(filepath.Base(path), "zz_") {
					helper = filepath.Base(path)
				}
			}
			if helper != tc.want.helper {
				t.Errorf("helper file %q, want %q", helper, tc.want.helper)
			}
			checkOverlay(t, pkg.Dir, files)
		})
	}
}

// checkOverlay requires the package in dir, with files laid over it, to
// type-check (its tests included), and every rewritten file to keep the line
// count of the original.
func checkOverlay(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	cfg := &packages.Config{
		Mode:    packages.NeedName | packages.NeedTypes | packages.NeedSyntax,
		Dir:     dir,
		Tests:   true,
		Overlay: files,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		t.Fatal(err)
	}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			t.Errorf("overlay does not type-check: %v", e)
		}
	})
	for path, content := range files {
		orig, err := os.ReadFile(path) //nolint:gosec // G304: a path RewritePackage returned, in a fixture
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(orig, content) {
			t.Errorf("%s returned unchanged", path)
		}
		if got, want := bytes.Count(beforeDuplicates(content), []byte("\n")), bytes.Count(orig, []byte("\n")); got != want {
			t.Errorf("%s: %d lines, original %d", path, got, want)
		}
	}
}

// mutantID returns the id of the mutant of type mt at the site with token tok.
func mutantID(t *testing.T, sites []schemata.Site, tok token.Token, mt mutator.Type) int {
	t.Helper()
	for _, s := range sites {
		if s.Tok != tok {
			continue
		}
		for _, m := range s.Muts {
			if m.Type == mt {
				return m.ID
			}
		}
	}
	t.Fatalf("no %s mutant at %s", mt, tok)

	return 0
}

// runTestBinary runs the test binary bin in dir with GREMLINS_MUTANT=id.
func runTestBinary(t *testing.T, bin, dir string, id int) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-test.timeout=50s")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GREMLINS_MUTANT="+strconv.Itoa(id), "GREMLINS_REACHED=")

	return cmd.CombinedOutput()
}

// TestBuildAll builds schema copies: every package that builds has a test
// binary running the schema, a package that does not build is reported
// without costing the others theirs, and the original module is untouched.
func TestBuildAll(t *testing.T) {
	t.Parallel()
	type pkgCase struct {
		pattern, importPath string
		broken              []byte // replaces the package's one source file
		wantErr             bool
		addTok              bool // the package has a + site whose mutant fails its test
	}
	testCases := map[string]struct {
		dir  string
		pkgs []pkgCase
	}{
		"prefix_taken": {
			dir:  "testdata/prefixclash",
			pkgs: []pkgCase{{pattern: ".", importPath: "prefixclash", addTok: true}},
		},
		"one_package_fails_build": {
			dir: "testdata/twopkgs",
			pkgs: []pkgCase{
				{pattern: "./ok", importPath: "twopkgs/ok", addTok: true},
				{pattern: "./bad", importPath: "twopkgs/bad", broken: []byte("package bad\n\nfunc F() int { return undefined }\n"), wantErr: true},
			},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			modRoot, err := filepath.Abs(tc.dir)
			if err != nil {
				t.Fatal(err)
			}
			rewritten := map[string]map[string][]byte{}
			ids := map[string]int{}
			var testPkgs, wantRewritten []string
			for _, p := range tc.pkgs {
				pkg := loadPkg(t, tc.dir, p.pattern)
				testPkgs = append(testPkgs, p.importPath)
				var files map[string][]byte
				if p.broken != nil {
					files = map[string][]byte{pkg.GoFiles[0]: p.broken}
				} else {
					sites := pkgSites(pkg)
					var dropped []schemata.SiteError
					files, _, dropped = schemata.RewritePackage(pkg, sites, "")
					if len(dropped) > 0 {
						t.Fatalf("%s: sites dropped: %v", p.importPath, dropped)
					}
					if p.addTok {
						ids[p.importPath] = mutantID(t, sites, token.ADD, mutator.ArithmeticBase)
					}
				}
				rewritten[p.importPath] = files
				for path := range files {
					rel, err := filepath.Rel(modRoot, path)
					if err != nil {
						t.Fatal(err)
					}
					wantRewritten = append(wantRewritten, rel)
				}
			}
			before := snapshot(t, modRoot)

			b, errs := schemata.BuildAll(context.Background(), modRoot, t.TempDir(), "", rewritten, testPkgs, 5*time.Minute)

			if after := snapshot(t, modRoot); !maps.Equal(before, after) {
				t.Error("BuildAll changed the original module")
			}
			slices.Sort(wantRewritten)
			if !slices.Equal(b.Rewritten, wantRewritten) {
				t.Errorf("Rewritten = %v, want %v", b.Rewritten, wantRewritten)
			}
			for _, p := range tc.pkgs {
				bin, built := b.Binaries[p.importPath]
				if gotErr := errs[p.importPath] != nil; gotErr != p.wantErr || built == p.wantErr {
					t.Errorf("%s: error %v, binary %q; want error %v", p.importPath, errs[p.importPath], bin, p.wantErr)

					continue
				}
				if p.wantErr {
					continue
				}
				pkgDir := filepath.Join(b.Dir, filepath.FromSlash(strings.TrimPrefix(p.pattern, "./")))
				if out, err := runTestBinary(t, bin, pkgDir, 0); err != nil {
					t.Errorf("%s with no mutant: %v\n%s", p.importPath, err, out)
				}
				if p.addTok {
					if out, err := runTestBinary(t, bin, pkgDir, ids[p.importPath]); err == nil {
						t.Errorf("%s with the + mutant active passed; the binary is not the schema\n%s", p.importPath, out)
					}
				}
			}
			for p, err := range errs {
				if !slices.ContainsFunc(tc.pkgs, func(c pkgCase) bool { return c.importPath == p }) {
					t.Errorf("error for unrequested package %q: %v", p, err)
				}
			}
		})
	}
}

// snapshot reads every file under dir.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // G304: a file under the fixture module
		files[path] = string(b)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return files
}

// TestBuildAllReportsMissingTestFiles checks that a package without tests
// is recorded as having none, with neither an error nor a binary path that
// does not exist.
func TestBuildAllReportsMissingTestFiles(t *testing.T) {
	t.Parallel()
	modRoot, err := filepath.Abs("testdata/oldgo")
	if err != nil {
		t.Fatal(err)
	}
	b, errs := schemata.BuildAll(context.Background(), modRoot, t.TempDir(), "", nil, []string{"oldgo"}, 5*time.Minute)
	if _, ok := b.Binaries["oldgo"]; ok || errs["oldgo"] != nil || !b.NoTests["oldgo"] {
		t.Errorf("binary %v, error %v, NoTests %v; want oldgo recorded as having no tests", b.Binaries, errs["oldgo"], b.NoTests)
	}
}

// TestBuildAllHonoursAllowance checks that the compile allowance bounds the
// build: an allowance already spent fails every package.
func TestBuildAllHonoursAllowance(t *testing.T) {
	t.Parallel()
	modRoot, err := filepath.Abs("testdata/twopkgs")
	if err != nil {
		t.Fatal(err)
	}
	_, errs := schemata.BuildAll(context.Background(), modRoot, t.TempDir(), "", nil, []string{"twopkgs/ok", "twopkgs/bad"}, time.Nanosecond)
	for _, p := range []string{"twopkgs/ok", "twopkgs/bad"} {
		if !errors.Is(errs[p], context.DeadlineExceeded) {
			t.Errorf("%s: error %v, want the deadline", p, errs[p])
		}
	}
}

// TestBinaryNamesDoNotCollide checks that import paths that sanitise alike
// still get distinct binaries.
func TestBinaryNamesDoNotCollide(t *testing.T) {
	t.Parallel()
	names := schemata.BinaryNames([]string{"a/b", "a_b", "a.b", "a/b/c", "a_b_c", "x"})
	seen := map[string]string{}
	for p, n := range names {
		if strings.ContainsAny(n, `/\`) {
			t.Errorf("%s: name %q holds a separator", p, n)
		}
		if q, dup := seen[n]; dup {
			t.Errorf("%s and %s share binary name %q", p, q, n)
		}
		seen[n] = p
	}
	if len(names) != 6 {
		t.Errorf("got %d names, want 6", len(names))
	}
}
