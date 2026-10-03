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
	"errors"
	"fmt"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-gremlins/gremlins/internal/engine"
	"github.com/go-gremlins/gremlins/internal/mutator"
	"github.com/go-gremlins/gremlins/internal/schemata"
)

// testPrefix is deliberately not "_g": a template that forgot to substitute
// the prefix leaves "_g" names behind, and the fixture's calls to testPrefix
// names then fail to compile.
const testPrefix = "_zz"

var allHelpers = []string{
	"Active", "Reached", "Bit", "Xor", "LSS", "LEQ", "GTR", "GEQ",
	"ADD", "SUB", "MUL", "QUO", "REM", "NEG", "POS", "IncDec", "IncDecMap",
	"AND", "OR", "XOR", "ANDNOT", "SHL", "SHR",
	"ADDAssign", "ADDAssignMap", "ADDAssignStr", "ADDAssignStrMap", "SUBAssign", "SUBAssignMap",
	"MULAssign", "MULAssignMap", "QUOAssign", "QUOAssignMap", "REMAssign", "REMAssignMap",
	"ANDAssign", "ANDAssignMap", "ORAssign", "ORAssignMap", "XORAssign", "XORAssignMap",
	"ANDNOTAssign", "ANDNOTAssignMap", "SHLAssign", "SHLAssignMap", "SHLAssignX", "SHLAssignXMap",
	"SHRAssign", "SHRAssignMap", "SHRAssignX", "SHRAssignXMap",
}

// siteKind is the shape of the call the fixture makes for a site.
type siteKind int

const (
	kindBinary    siteKind = iota // helper(ids..., l, r) vs l tok r
	kindUnary                     // helper(ids..., x) vs tok x
	kindXor                       // Xor(id, l tok r) vs l tok r
	kindIncDec                    // IncDec(id, &v, inc) vs v tok
	kindIncDecMap                 // IncDecMap(id, m, k, inc) vs m[k] tok
	kindBit                       // Bit(id) vs 1 when active, else 0
)

// site is one helper call in the fixture. muts lists the mutator types in the
// helper's id-parameter order; zero passes 0 for every id (mutant absent).
type site struct {
	helper   string
	kind     siteKind
	tok      token.Token
	operands string // fixture variable holding the operand table
	wrap     string // optional conversion applied to Xor's operand
	muts     []mutator.Type
	zero     bool
}

func helperSites() []site {
	var sites []site
	ordered := map[token.Token]string{token.LSS: "LSS", token.LEQ: "LEQ", token.GTR: "GTR", token.GEQ: "GEQ"}
	cmpTypes := []mutator.Type{mutator.ConditionalsBoundary, mutator.ConditionalsNegation}
	for _, tok := range []token.Token{token.LSS, token.LEQ, token.GTR, token.GEQ} {
		for _, ops := range []string{"ordI", "ordF", "ordN"} {
			sites = append(sites, site{helper: ordered[tok], kind: kindBinary, tok: tok, operands: ops, muts: cmpTypes})
		}
		sites = append(sites, site{helper: ordered[tok], kind: kindBinary, tok: tok, operands: "ordI", muts: cmpTypes, zero: true})
	}
	arith := []struct {
		helper string
		tok    token.Token
		ops    []string
	}{
		{"ADD", token.ADD, []string{"ordI", "ordF", "ordN"}},
		{"SUB", token.SUB, []string{"ordI", "ordF"}},
		{"MUL", token.MUL, []string{"nzI", "nzF"}}, // MUL's mutant divides
		{"QUO", token.QUO, []string{"nzI", "nzF"}},
		{"REM", token.REM, []string{"nzI"}},
	}
	ab := []mutator.Type{mutator.ArithmeticBase}
	// The engine discovers INVERT_NEGATIVES at a binary - as well as at a
	// unary one, so SUB carries two ids, as NEG does.
	neg := []mutator.Type{mutator.ArithmeticBase, mutator.InvertNegatives}
	for _, a := range arith {
		muts := ab
		if a.tok == token.SUB {
			muts = neg
		}
		for _, ops := range a.ops {
			sites = append(sites, site{helper: a.helper, kind: kindBinary, tok: a.tok, operands: ops, muts: muts})
		}
		sites = append(sites, site{helper: a.helper, kind: kindBinary, tok: a.tok, operands: a.ops[0], muts: muts, zero: true})
	}
	ib := []mutator.Type{mutator.InvertBitwise}
	bitwise := []struct {
		helper string
		tok    token.Token
	}{
		{"AND", token.AND}, {"OR", token.OR}, {"XOR", token.XOR}, {"ANDNOT", token.AND_NOT},
		{"SHL", token.SHL}, {"SHR", token.SHR},
	}
	for _, bw := range bitwise {
		for _, ops := range []string{"bitI", "bitU", "bitN"} {
			sites = append(sites, site{helper: bw.helper, kind: kindBinary, tok: bw.tok, operands: ops, muts: ib})
		}
		sites = append(sites, site{helper: bw.helper, kind: kindBinary, tok: bw.tok, operands: "bitI", muts: ib, zero: true})
	}
	// unU: unary minus and plus are legal Go on unsigned operands too.
	for _, ops := range []string{"unI", "unF", "unU"} {
		sites = append(sites,
			site{helper: "NEG", kind: kindUnary, tok: token.SUB, operands: ops, muts: neg},
			site{helper: "POS", kind: kindUnary, tok: token.ADD, operands: ops, muts: ab})
	}
	sites = append(sites,
		site{helper: "NEG", kind: kindUnary, tok: token.SUB, operands: "unI", muts: neg, zero: true},
		site{helper: "POS", kind: kindUnary, tok: token.ADD, operands: "unI", muts: ab, zero: true})
	cn := []mutator.Type{mutator.ConditionalsNegation}
	for _, tok := range []token.Token{token.EQL, token.NEQ} {
		for _, ops := range []string{"ordI", "ordF"} {
			sites = append(sites, site{helper: "Xor", kind: kindXor, tok: tok, operands: ops, muts: cn})
		}
		sites = append(sites,
			site{helper: "Xor", kind: kindXor, tok: tok, operands: "ordI", wrap: "myBool", muts: cn},
			site{helper: "Xor", kind: kindXor, tok: tok, operands: "ordI", muts: cn, zero: true})
	}
	id := []mutator.Type{mutator.IncrementDecrement}
	for _, tok := range []token.Token{token.INC, token.DEC} {
		for _, ops := range []string{"unI", "unF"} {
			sites = append(sites, site{helper: "IncDec", kind: kindIncDec, tok: tok, operands: ops, muts: id})
		}
		sites = append(sites,
			site{helper: "IncDec", kind: kindIncDec, tok: tok, operands: "unI", muts: id, zero: true},
			site{helper: "IncDecMap", kind: kindIncDecMap, tok: tok, operands: "unI", muts: id},
			site{helper: "IncDecMap", kind: kindIncDecMap, tok: tok, operands: "unI", muts: id, zero: true})
	}
	sites = append(sites,
		site{helper: "Bit", kind: kindBit, muts: []mutator.Type{mutator.ArithmeticBase}},
		site{helper: "Bit", kind: kindBit, muts: []mutator.Type{mutator.ArithmeticBase}, zero: true})

	return sites
}

const fixtureOperands = `
type myInt int
type myBool bool

var (
	ordI = [][2]int{{3, 5}, {5, 3}, {5, 5}, {-2, 7}, {0, 0}}
	ordF = [][2]float64{{3, 5}, {5, 3}, {5, 5}, {-2, 7}, {0, 0}, {math.NaN(), 1}, {1, math.NaN()}, {math.NaN(), math.NaN()}}
	ordN = [][2]myInt{{3, 5}, {5, 3}, {5, 5}, {-2, 7}, {0, 0}}
	nzI  = [][2]int{{3, 5}, {5, 3}, {5, 5}, {-2, 7}}
	nzF  = [][2]float64{{3, 5}, {5, 3}, {5, 5}, {-2, 7}}
	unI  = []int{3, -2, 0, 5}
	unF  = []float64{3, -2, 0, 5}
	unU  = []uint8{3, 0, 200}
	bitI = [][2]int{{12, 10}, {0, 7}, {255, 1}, {-6, 3}}
	bitU = [][2]uint8{{12, 10}, {0, 7}, {255, 1}}
	bitN = [][2]myInt{{12, 10}, {0, 7}, {255, 1}, {-6, 3}}
)
`

// fixtureMain writes a main that, for every site and operand, prints the
// helper's result, the original operator's result and each mutated
// operator's result (the operators applied directly, mutated tokens taken
// from engine.TokenMutation). It returns each site's mutant ids.
func fixtureMain(t *testing.T, sites []site) (string, [][]int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("package main\n\nimport (\n\t\"fmt\"\n\t\"math\"\n)\n")
	b.WriteString(fixtureOperands)
	b.WriteString("\nfunc main() {\n")
	ids := make([][]int, len(sites))
	next := 1
	for si, s := range sites {
		args := make([]string, len(s.muts))
		for i := range s.muts {
			if s.zero {
				args[i] = "0"

				continue
			}
			ids[si] = append(ids[si], next)
			args[i] = strconv.Itoa(next)
			next++
		}
		idArgs := strings.Join(args, ", ")
		toks := []token.Token{s.tok}
		if s.kind != kindBit {
			for _, mt := range s.muts {
				to, ok := engine.TokenMutation(mt, s.tok)
				if !ok {
					t.Fatalf("site %d: engine has no %s mutation for %s", si, mt, s.tok)
				}
				toks = append(toks, to)
			}
		}
		exprs := make([]string, 0, len(toks)+1)
		h := testPrefix + s.helper
		switch s.kind {
		case kindBinary:
			exprs = append(exprs, fmt.Sprintf("%s(%s, p[0], p[1])", h, idArgs))
			for _, tk := range toks {
				exprs = append(exprs, fmt.Sprintf("p[0] %s p[1]", tk))
			}
		case kindXor:
			wrap := func(e string) string {
				if s.wrap == "" {
					return e
				}

				return s.wrap + "(" + e + ")"
			}
			exprs = append(exprs, fmt.Sprintf("%s(%s, %s)", h, idArgs, wrap(fmt.Sprintf("p[0] %s p[1]", s.tok))))
			for _, tk := range toks {
				exprs = append(exprs, wrap(fmt.Sprintf("p[0] %s p[1]", tk)))
			}
		case kindUnary:
			exprs = append(exprs, fmt.Sprintf("%s(%s, p)", h, idArgs))
			for _, tk := range toks {
				exprs = append(exprs, fmt.Sprintf("%sp", tk))
			}
		case kindIncDec:
			inc := s.tok == token.INC
			exprs = append(exprs, fmt.Sprintf("func() any { v := p; %s(%s, &v, %t); return v }()", h, idArgs, inc))
			for _, tk := range toks {
				exprs = append(exprs, fmt.Sprintf("func() any { v := p; v%s; return v }()", tk))
			}
		case kindIncDecMap:
			inc := s.tok == token.INC
			exprs = append(exprs, fmt.Sprintf(
				"func() any { m := map[string]int{\"k\": p}; %s(%s, m, \"k\", %t); return m[\"k\"] }()", h, idArgs, inc))
			for _, tk := range toks {
				exprs = append(exprs, fmt.Sprintf("func() any { m := map[string]int{\"k\": p}; m[\"k\"]%s; return m[\"k\"] }()", tk))
			}
		case kindBit:
			// Bit has no token: the original is 0 and the mutant is 1.
			fmt.Fprintf(&b, "\tfmt.Println(%d, 0, %s(%s), 0, 1)\n", si, h, idArgs)

			continue
		}
		fmt.Fprintf(&b, "\tfor i, p := range %s {\n\t\tfmt.Println(%d, i, %s)\n\t}\n", s.operands, si, strings.Join(exprs, ", "))
	}
	b.WriteString("}\n")

	return b.String(), ids
}

// writeModule creates a throwaway module holding files and returns its dir.
func writeModule(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = []byte("module fixture\n\ngo 1.22\n")
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// goCmd runs the go tool in dir with a clean module environment.
func goCmd(t *testing.T, dir string, env []string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("go", args...) //nolint:gosec // G204: the args are this test's own literals
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GREMLINS_MUTANT=", "GREMLINS_REACHED=")
	cmd.Env = append(cmd.Env, env...)

	return cmd.CombinedOutput()
}

// TestHelpersMatchTokenMutations runs every helper with its mutant inactive,
// absent (id 0, with GREMLINS_MUTANT unset, "0" and unparseable) and active,
// and requires the result to equal the original operator, or -- for exactly
// the active id -- the operator engine.TokenMutation rewrites it to. It also
// requires the reach file to appear exactly when some site's mutant is active.
func TestHelpersMatchTokenMutations(t *testing.T) {
	t.Parallel()
	sites := helperSites()
	for i, s := range sites {
		if s.kind == kindBit || s.zero {
			continue
		}
		want := slices.Clone(engine.TokenMutantType[s.tok])
		got := slices.Clone(s.muts)
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("site %d (%s %s): helper carries %v, engine discovers %v", i, s.helper, s.tok, got, want)
		}
	}
	main, ids := fixtureMain(t, sites)
	h := &schemata.HelperSet{}
	for _, name := range allHelpers {
		h.Use(name)
	}
	dir := writeModule(t, map[string][]byte{"main.go": []byte(main), "helpers.go": helperFile(t, h, "main", testPrefix)})
	bin := filepath.Join(dir, "fixture")
	if out, err := goCmd(t, dir, nil, "build", "-o", bin, "."); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	maxID := 0
	for _, s := range ids {
		for _, id := range s {
			maxID = max(maxID, id)
		}
	}
	runs := map[string]string{"unset": "", "zero": "0", "garbage": "x1"}
	for id := 1; id <= maxID; id++ {
		runs["id"+strconv.Itoa(id)] = strconv.Itoa(id)
	}
	for name, env := range runs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			active, _ := strconv.Atoi(env)
			reach := filepath.Join(t.TempDir(), "reached")
			cmd := exec.Command(bin)
			cmd.Env = append(os.Environ(), "GREMLINS_MUTANT="+env, "GREMLINS_REACHED="+reach)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			checkLines(t, sites, ids, active, out)
			_, statErr := os.Stat(reach)
			if reached := statErr == nil; reached != (active >= 1 && active <= maxID) {
				t.Errorf("reach file exists = %v with GREMLINS_MUTANT=%q", reached, env)
			}
		})
	}
}

func checkLines(t *testing.T, sites []site, ids [][]int, active int, out []byte) {
	t.Helper()
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		si, err := strconv.Atoi(f[0])
		if err != nil || len(f) < 4 {
			t.Fatalf("bad line %q", line)
		}
		lines++
		s := sites[si]
		want := f[3] // the original operator
		for i, id := range ids[si] {
			if id == active {
				want = f[4+i]
			}
		}
		if f[2] != want {
			t.Errorf("site %d (%s %s on %s, zero=%v) operand %s: helper gave %s, want %s (line %q)",
				si, s.helper, s.tok, s.operands, s.zero, f[1], f[2], want, line)
		}
	}
	if lines == 0 {
		t.Fatal("fixture printed nothing")
	}
}

// TestReachedIsRaceFree calls a helper on the active id from 50 goroutines
// under the race detector: the reach file must exist and no race be reported.
func TestReachedIsRaceFree(t *testing.T) {
	t.Parallel()
	h := &schemata.HelperSet{}
	h.Use("Xor")
	main := `package main

import "sync"

func main() {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ` + testPrefix + `Xor(1, true)
		}()
	}
	wg.Wait()
}
`
	dir := writeModule(t, map[string][]byte{"main.go": []byte(main), "helpers.go": helperFile(t, h, "main", testPrefix)})
	reach := filepath.Join(t.TempDir(), "reached")
	out, err := goCmd(t, dir, []string{"CGO_ENABLED=1", "GREMLINS_MUTANT=1", "GREMLINS_REACHED=" + reach}, "run", "-race", ".")
	if err != nil || bytes.Contains(out, []byte("DATA RACE")) {
		t.Fatalf("race run failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(reach); err != nil {
		t.Errorf("reach file missing: %v", err)
	}
}

// TestHelperFileCompilesWithOnlyUsedHelpers checks that File emits only what
// Use asked for, imports only what that needs, and vets clean either way.
func TestHelperFileCompilesWithOnlyUsedHelpers(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		use     []string
		raw     []string
		present []string
		absent  []string
		user    string
	}{
		"xor_only": {
			use:     []string{"Xor"},
			present: []string{"func _zzXor[", "func _zzReached(", "var _zzActive ="},
			absent:  []string{`"cmp"`, "_zzNumber", "_zzLSS", "_zzBit", "func _g", "_gActive", "\t\"os\""},
		},
		"ordered_only": {
			use:     []string{"GEQ"},
			present: []string{`"cmp"`, "func _zzGEQ["},
			absent:  []string{"_zzNumber", "_zzLSS"},
		},
		"rem_only": {
			use:     []string{"REM"},
			present: []string{"type _zzInteger interface", "func _zzREM["},
			absent:  []string{`"cmp"`, "_zzNumber"},
		},
		"nothing_used": {
			present: []string{"var _zzActive =", "func _zzReached("},
			absent:  []string{`"cmp"`, "_zzXor"},
		},
		"raw": {
			use:     []string{"Bit"},
			raw:     []string{"func _zzRaw7() bool {\n\tif _zzActive != 0 && _zzActive == 7 { _zzReached(); return false }\n\treturn true\n}"},
			present: []string{"func _zzRaw7() bool", "func _zzBit("},
			absent:  []string{`"cmp"`},
		},
		"all": {
			use: allHelpers,
			// A user package may declare cmp, os, sync or strconv itself.
			user:    "package fixture\n\nfunc cmp() {}\n\nvar os, sync, strconv = 1, 2, 3\n",
			present: []string{`"cmp"`, "_zzNumber", "_zzInteger", "func _zzIncDecMap["},
			absent:  []string{"_zzSigned"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := &schemata.HelperSet{}
			for _, n := range tc.use {
				h.Use(n)
			}
			for _, r := range tc.raw {
				h.AddRaw(r)
			}
			src := helperFile(t, h, "fixture", testPrefix)
			if !bytes.HasPrefix(src, []byte("// Code generated by gremlins schemata. DO NOT EDIT.\n\npackage fixture\n")) {
				t.Errorf("missing generated header or package clause:\n%s", src)
			}
			for _, s := range tc.present {
				if !bytes.Contains(src, []byte(s)) {
					t.Errorf("missing %q in:\n%s", s, src)
				}
			}
			for _, s := range tc.absent {
				if bytes.Contains(src, []byte(s)) {
					t.Errorf("unexpected %q in:\n%s", s, src)
				}
			}
			files := map[string][]byte{"helpers.go": src}
			if tc.user != "" {
				files["user.go"] = []byte(tc.user)
			}
			dir := writeModule(t, files)
			if out, err := goCmd(t, dir, nil, "vet", "."); err != nil {
				t.Errorf("go vet: %v\n%s\n%s", err, out, src)
			}
		})
	}
}

// helperFile returns h's file, failing the test if it does not format.
func helperFile(t *testing.T, h *schemata.HelperSet, pkgName, prefix string) []byte {
	t.Helper()
	src, err := h.File(pkgName, prefix)
	if err != nil {
		t.Fatalf("helper file: %v\n%s", err, src)
	}

	return src
}

// TestHelperFileReportsUnparsableRaw checks that AddRaw code that does not
// parse is an error of File, with the unformatted source returned for the
// message, rather than a file handed on to fail at compile time.
func TestHelperFileReportsUnparsableRaw(t *testing.T) {
	t.Parallel()
	h := &schemata.HelperSet{}
	h.AddRaw("func _zzBroken( {")
	src, err := h.File("p", testPrefix)
	if err == nil {
		t.Fatalf("File accepted unparsable raw code:\n%s", src)
	}
	if !bytes.Contains(src, []byte("func _zzBroken( {")) {
		t.Errorf("unformatted source not returned:\n%s", src)
	}
}

// TestAddRawDeduplicates checks that the same raw declaration added twice
// -- as two sites needing one shared helper do -- is declared once.
func TestAddRawDeduplicates(t *testing.T) {
	t.Parallel()
	h := &schemata.HelperSet{}
	code := "func _zzOne() int { return 1 }"
	h.AddRaw(code)
	h.AddRaw(code)
	src := helperFile(t, h, "p", testPrefix)
	if n := bytes.Count(src, []byte("func _zzOne(")); n != 1 {
		t.Errorf("declared %d times:\n%s", n, src)
	}
}

// TestHelperFileIsDeterministic checks that output depends on the set of
// helpers used, not on the order or repetition of Use calls.
func TestHelperFileIsDeterministic(t *testing.T) {
	t.Parallel()
	a, b := &schemata.HelperSet{}, &schemata.HelperSet{}
	for _, n := range []string{"ADD", "Xor", "LSS"} {
		a.Use(n)
	}
	for _, n := range []string{"LSS", "Xor", "ADD", "Xor", "Active"} {
		b.Use(n)
	}
	if fa, fb := helperFile(t, a, "p", "_g"), helperFile(t, b, "p", "_g"); !bytes.Equal(fa, fb) {
		t.Errorf("Use order changed the file:\n%s\n---\n%s", fa, fb)
	}
}

// TestUseRejectsUnknownHelper checks that a misspelt helper name fails loudly
// at generation time rather than as an undefined name at compile time.
func TestUseRejectsUnknownHelper(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		err, ok := r.(error)
		if !ok || !errors.Is(err, schemata.ErrUnknownHelper) {
			t.Errorf("recovered %v, want ErrUnknownHelper", r)
		}
	}()
	(&schemata.HelperSet{}).Use("Lss")
}
