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
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/go-gremlins/gremlins/internal/engine"
	"github.com/go-gremlins/gremlins/internal/mutator"
	"github.com/go-gremlins/gremlins/internal/schemata"
)

// assignMutators are the mutators with a statement form.
var assignMutators = []mutator.Type{
	mutator.InvertAssignments, mutator.InvertBitwiseAssignments, mutator.RemoveSelfAssignments,
}

// stmtMutators are the mutators TestStmtFormsBehave discovers: every one
// with a form, so that expression sites nest inside statement sites.
var stmtMutators = slices.Concat(formsMutators, assignMutators)

// plainSource is src with p's token rewritten as the engine rewrites it.
func plainSource(t *testing.T, src []byte, fset *token.FileSet, p plainMutant) []byte {
	t.Helper()
	to, ok := engine.TokenMutation(p.mt, p.tok)
	if !ok {
		t.Fatalf("engine has no %s mutation for %s", p.mt, p.tok)
	}
	off := fset.Position(p.pos).Offset

	return slices.Concat(src[:off:off], []byte(to.String()), src[off+len(p.tok.String()):])
}

// viable drops the mutants whose plain source does not type-check, as the
// engine's TypeViability does before generating them, and returns the
// dropped ones.
func viable(t *testing.T, src []byte, fset *token.FileSet, sites []schemata.Site, plain []plainMutant) ([]schemata.Site, []plainMutant, []plainMutant) {
	t.Helper()
	dropped := map[int]bool{}
	var kept, gone []plainMutant
	for _, p := range plain {
		mfset := token.NewFileSet()
		f, err := parser.ParseFile(mfset, "stmt.go", plainSource(t, src, fset, p), 0)
		if err == nil {
			conf := types.Config{Importer: importer.Default()}
			_, err = conf.Check("fixture", mfset, []*ast.File{f}, nil)
		}
		if err != nil {
			dropped[p.id] = true
			gone = append(gone, p)

			continue
		}
		kept = append(kept, p)
	}
	var out []schemata.Site
	for _, s := range sites {
		s.Muts = slices.DeleteFunc(slices.Clone(s.Muts), func(m schemata.Mutant) bool { return dropped[m.ID] })
		if len(s.Muts) > 0 {
			out = append(out, s)
		}
	}

	return out, kept, gone
}

// TestStmtFormsBehave is TestFormsBehave for the statement forms: every
// site of testdata/stmt, of every mutator with a form, rendered into one
// schema binary that must print, under each mutant id, exactly what the plain
// token mutant prints, and record that the site was reached.
func TestStmtFormsBehave(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("testdata/stmt/stmt.go")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := os.ReadFile("testdata/stmt/stmt_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, info := typeCheck(t, fset, "stmt.go", src)
	df, err := parser.ParseFile(fset, "stmt_test.go", driver, 0)
	if err != nil {
		t.Fatal(err)
	}
	sites, plain := discoverFor(f, stmtMutators)
	sites, plain, gone := viable(t, src, fset, sites, plain)
	covered := map[mutator.Type]bool{}
	for _, p := range plain {
		covered[p.mt] = true
	}
	for _, mt := range assignMutators {
		if !covered[mt] {
			t.Errorf("fixture has no %s mutant", mt)
		}
	}
	// The fixture must hold the sites a mutant is missing from: s -= t on a
	// string, and x = n for a count of another type than the value.
	var goneAssign []string
	for _, p := range gone {
		if slices.Contains(assignMutators, p.mt) {
			goneAssign = append(goneAssign, fmt.Sprintf("%s %s line %d", p.mt, p.tok, fset.Position(p.pos).Line))
		}
	}
	if len(goneAssign) == 0 {
		t.Error("no assignment mutant was dropped as not viable")
	}
	t.Logf("not viable: %v", goneAssign)

	prefix := schemata.ChoosePrefix([]*ast.File{f, df})
	h := &schemata.HelperSet{}
	out, errs := schemata.Render(fset, fset.File(f.Pos()), src, sites, schemata.NewRewriter(info, []*ast.File{f}, prefix, h))
	for _, e := range errs {
		t.Errorf("site at %s not rewritten: %v", fset.Position(e.Site.Node.Pos()), e.Err)
	}
	if t.Failed() {
		t.FailNow()
	}
	if got, want := bytes.Count(out, []byte("\n")), bytes.Count(src, []byte("\n")); got != want {
		t.Fatalf("rewrite has %d lines, original %d", got, want)
	}
	schemaBin := buildFixture(t, map[string][]byte{
		"stmt.go": out, "stmt_test.go": driver, "gremlins_schemata.go": helperFile(t, h, "stmt", prefix),
	})
	origBin := buildFixture(t, map[string][]byte{"stmt.go": src, "stmt_test.go": driver})

	t.Run("id0", func(t *testing.T) {
		t.Parallel()
		reach := filepath.Join(t.TempDir(), "reached")
		want := runFixture(t, origBin)
		// The fixture must exercise what it is there for: each side effect
		// once, the label taken, the closures per iteration.
		for _, line := range []string{
			"AddAssignSideEffectLHS: [[1 4 3] 1]\n", "AddAssignSideEffectRHS: [6 1]\n",
			"MapAssignNamedMap: [map[a:9 b:9] 2]\n", "LabelledAssign: [4 13]\n", "ForPostAddAssign: [0 3 6 9]\n",
		} {
			if !bytes.Contains(want, []byte(line)) {
				t.Errorf("original output lacks %q:\n%s", line, want)
			}
		}
		if got := runFixture(t, schemaBin, "GREMLINS_REACHED="+reach); !bytes.Equal(got, want) {
			t.Errorf("schema without a mutant printed\n%s\nthe original printed\n%s", got, want)
		}
		if _, err := os.Stat(reach); err == nil {
			t.Error("reach file written with no mutant active")
		}
	})
	for _, p := range plain {
		name := fmt.Sprintf("id%d_%s_%s_line%d", p.id, p.mt, p.tok, fset.Position(p.pos).Line)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := runFixture(t, buildFixture(t, map[string][]byte{"stmt.go": plainSource(t, src, fset, p), "stmt_test.go": driver}))
			reach := filepath.Join(t.TempDir(), "reached")
			got := runFixture(t, schemaBin, "GREMLINS_MUTANT="+strconv.Itoa(p.id), "GREMLINS_REACHED="+reach)
			if !bytes.Equal(got, want) {
				t.Errorf("schema with mutant %d printed\n%s\nthe plain mutant printed\n%s", p.id, got, want)
			}
			if _, err := os.Stat(reach); err != nil {
				t.Errorf("reach file missing: %v", err)
			}
		})
	}
}

// TestAssignMutationsMatchEngine holds the statement forms' table to the
// engine's: for every assignment mutator and every token, both rewrite it to
// the same token, or neither applies.
func TestAssignMutationsMatchEngine(t *testing.T) {
	t.Parallel()
	for _, mt := range assignMutators {
		for tok := token.ILLEGAL; tok <= token.TILDE; tok++ {
			want, wok := engine.TokenMutation(mt, tok)
			got, gok := schemata.AssignMutation(mt, tok)
			if got != want || gok != wok {
				t.Errorf("%s %s: statement forms rewrite to %s (%v), engine to %s (%v)", mt, tok, got, gok, want, wok)
			}
		}
	}
}

// TestNewRewriterAssign is TestNewRewriter for the op= statement forms.
func TestNewRewriterAssign(t *testing.T) {
	t.Parallel()
	ia := mutator.InvertAssignments
	ib := mutator.InvertBitwiseAssignments
	rs := mutator.RemoveSelfAssignments
	cases := map[string]rewriterCase{
		"add_block":         {src: "func f(x, y int) { x += y }", tok: token.ADD_ASSIGN, muts: []mutator.Type{ia, rs}, want: "switch _zzActive { case 1: _zzReached(); x -= y; case 2: _zzReached(); x = y; default: x += y }"},
		"rem_block_remove":  {src: "func f(x, y int) { x %= y }", tok: token.REM_ASSIGN, muts: []mutator.Type{rs}, want: "switch _zzActive { case 1: _zzReached(); x = y; default: x %= y }"},
		"and_block":         {src: "func f(x, y int) { x &= /* c */ y }", tok: token.AND_ASSIGN, muts: []mutator.Type{ib, rs}, want: "switch _zzActive { case 2: _zzReached(); x = /* c */ y; case 1: _zzReached(); x |= /* c */ y; default: x &= /* c */ y }"},
		"andnot_block":      {src: "func f(x, y int) { x &^= y }", tok: token.AND_NOT_ASSIGN, muts: []mutator.Type{ib}, want: "switch _zzActive { case 1: _zzReached(); x &= y; default: x &^= y }"},
		"string_block":      {src: "func f(s, t string) { s += t }", tok: token.ADD_ASSIGN, muts: []mutator.Type{rs}, want: "switch _zzActive { case 1: _zzReached(); s = t; default: s += t }"},
		"generic_block":     {src: "func f[T ~int | ~string](x, y T) { x += y }", tok: token.ADD_ASSIGN, muts: []mutator.Type{rs}, want: "switch _zzActive { case 1: _zzReached(); x = y; default: x += y }"},
		"case_body":         {src: "func f(x int) { switch { case x > 0: x += 1 } }", tok: token.ADD_ASSIGN, muts: []mutator.Type{ia}, want: "switch _zzActive { case 1: _zzReached(); x -= 1; default: x += 1 }"},
		"comm_body":         {src: "func f(x int) { select { default: x *= 2 } }", tok: token.MUL_ASSIGN, muts: []mutator.Type{ia}, want: "switch _zzActive { case 1: _zzReached(); x /= 2; default: x *= 2 }"},
		"labelled":          {src: "func f(x int) int { goto L\nL:\n\tx -= 1\n\treturn x }", tok: token.SUB_ASSIGN, muts: []mutator.Type{ia}, want: "switch _zzActive { case 1: _zzReached(); x += 1; default: x -= 1 }"},
		"block_multiline":   {src: "func f(x, y int) {\n\tx +=\n\t\ty\n}", tok: token.ADD_ASSIGN, muts: []mutator.Type{ia, rs}, want: "_zzADDAssign(1, 2, &x,\n\t\ty)"},
		"for_post":          {src: "func f() { for i := 0; i < 3; i += 2 {} }", tok: token.ADD_ASSIGN, muts: []mutator.Type{ia, rs}, want: "_zzADDAssign(1, 2, &i, 2)"},
		"for_init":          {src: "func f(x float64) { for x /= 2; ; { break } }", tok: token.QUO_ASSIGN, muts: []mutator.Type{ia}, want: "_zzQUOAssign(1, 0, &x, 2)"},
		"if_init":           {src: "func f(x int) bool { if x %= 3; x > 0 { return true }; return false }", tok: token.REM_ASSIGN, muts: []mutator.Type{rs, ia}, want: "_zzREMAssign(2, 1, &x, 3)"},
		"switch_init_map":   {src: "func f(m map[string]uint, k string) { switch m[k] ^= 1; {} }", tok: token.XOR_ASSIGN, muts: []mutator.Type{rs, ib}, want: "_zzXORAssignMap(1, 2, m, k, 1)"},
		"type_switch_init":  {src: "func f(x int, v any) { switch x |= 1; v.(type) {} }", tok: token.OR_ASSIGN, muts: []mutator.Type{ib}, want: "_zzORAssign(0, 1, &x, 1)"},
		"simple_string":     {src: "func f(s string) { if s += \"x\"; s != \"\" {} }", tok: token.ADD_ASSIGN, muts: []mutator.Type{rs}, want: "_zzADDAssignStr(1, &s, \"x\")"},
		"simple_string_map": {src: "type ns map[int]string\nfunc f(m ns) { if m[0] += \"x\"; true {} }", tok: token.ADD_ASSIGN, muts: []mutator.Type{rs}, want: "_zzADDAssignStrMap(1, m, 0, \"x\")"},
		"simple_shl_same":   {src: "func f(x, n uint) { for ; x < 9; x <<= n {} }", tok: token.SHL_ASSIGN, muts: []mutator.Type{rs, ib}, want: "_zzSHLAssign(1, 2, &x, n)"},
		"simple_shl_const":  {src: "func f(x int8) { for ; x < 9; x <<= 1 {} }", tok: token.SHL_ASSIGN, muts: []mutator.Type{rs, ib}, want: "_zzSHLAssign(1, 2, &x, 1)"},
		"simple_shr_mixed":  {src: "func f(x uint64, n uint8) { for ; x > 0; x >>= n {} }", tok: token.SHR_ASSIGN, muts: []mutator.Type{ib}, want: "_zzSHRAssignX(1, &x, n)"},
		"simple_shl_map":    {src: "func f(m map[int]int, n uint8) { for ; m[0] < 9; m[0] <<= n {} }", tok: token.SHL_ASSIGN, muts: []mutator.Type{ib}, want: "_zzSHLAssignXMap(1, m, 0, n)"},
		"simple_named":      {src: "type c float32\nfunc f(x c) { if x *= 2; x > 0 {} }", tok: token.MUL_ASSIGN, muts: []mutator.Type{ia, rs}, want: "_zzMULAssign(1, 2, &x, 2)"},
		"nested_lhs":        {src: "func f(a []int, i int) { for ; ; a[i+1] -= 1 {} }", tok: token.SUB_ASSIGN, muts: []mutator.Type{ia}, want: "_zzSUBAssign(1, 0, &a[i+1], 1)"},
		// Refused: a mutant whose arm would not compile, a foreign mutator,
		// and an operand neither form can reach.
		"string_invert":       {src: "func f(s, t string) { s += t }", tok: token.ADD_ASSIGN, muts: []mutator.Type{ia, rs}, refusal: "INVERT_ASSIGNMENTS"},
		"generic_invert":      {src: "func f[T ~int | ~string](x, y T) { x += y }", tok: token.ADD_ASSIGN, muts: []mutator.Type{ia}, refusal: "INVERT_ASSIGNMENTS"},
		"shl_mixed_remove":    {src: "func f(x uint64, n uint8) { x <<= n }", tok: token.SHL_ASSIGN, muts: []mutator.Type{rs}, refusal: "REMOVE_SELF_ASSIGNMENTS"},
		"shl_float_count":     {src: "func f(x int) { for ; ; x <<= 2.0 {} }", tok: token.SHL_ASSIGN, muts: []mutator.Type{ib}},
		"wrong_mutator":       {src: "func f(x, y int) { x %= y }", tok: token.REM_ASSIGN, muts: []mutator.Type{ib}},
		"generic_map_simple":  {src: "func f[M ~map[string]int](m M) { for ; ; m[\"a\"] += 1 {} }", tok: token.ADD_ASSIGN, muts: []mutator.Type{ia}, refusal: "op-assign in a simple statement, neither addressable nor a map entry"},
		"generic_map_multiln": {src: "func f[M ~map[string]int](m M) {\n\tm[\"a\"] +=\n\t\t1\n}", tok: token.ADD_ASSIGN, muts: []mutator.Type{ia}, refusal: "neither addressable nor a map entry"},
		"simple_generic_str":  {src: "func f[T ~int | ~string](x, y T) { for ; ; x += y {} }", tok: token.ADD_ASSIGN, muts: []mutator.Type{rs}},
		"no_files":            {src: "func f(x, y int) { x += y }", tok: token.ADD_ASSIGN, muts: []mutator.Type{ia}, noFiles: true},
	}
	runRewriterCases(t, cases)
}
