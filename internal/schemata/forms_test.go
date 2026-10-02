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
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/go-gremlins/gremlins/internal/engine"
	"github.com/go-gremlins/gremlins/internal/mutator"
	"github.com/go-gremlins/gremlins/internal/schemata"
)

// formsMutators are the mutators the schemata prototype rewrites.
var formsMutators = []mutator.Type{
	mutator.ArithmeticBase, mutator.ConditionalsBoundary, mutator.ConditionalsNegation,
	mutator.IncrementDecrement, mutator.InvertNegatives,
}

// plainMutant is one mutant as the engine applies it: tok at pos becomes the
// token engine.TokenMutation maps it to.
type plainMutant struct {
	id  int
	pos token.Pos
	tok token.Token
	mt  mutator.Type
}

// discover finds the sites the engine would, restricted to formsMutators,
// and numbers their mutants from 1 in source position order.
func discover(f *ast.File) ([]schemata.Site, []plainMutant) {
	type found struct {
		node ast.Node
		pos  token.Pos
		tok  token.Token
		mts  []mutator.Type
	}
	var all []found
	ast.Inspect(f, func(n ast.Node) bool {
		tn, ok := engine.NewTokenNode(n)
		if !ok {
			return true
		}
		mts, ok := engine.MutantTypesFor(tn)
		if !ok {
			return true
		}
		var keep []mutator.Type
		for _, mt := range mts {
			if slices.Contains(formsMutators, mt) {
				keep = append(keep, mt)
			}
		}
		if len(keep) > 0 {
			all = append(all, found{node: n, pos: tn.TokPos, tok: tn.Tok(), mts: keep})
		}

		return true
	})
	slices.SortFunc(all, func(a, b found) int { return int(a.pos) - int(b.pos) })
	var sites []schemata.Site
	var plain []plainMutant
	for _, s := range all {
		site := schemata.Site{Node: s.node, Tok: s.tok}
		for _, mt := range s.mts {
			id := len(plain) + 1
			site.Muts = append(site.Muts, schemata.Mutant{ID: id, Type: mt})
			plain = append(plain, plainMutant{id: id, pos: s.pos, tok: s.tok, mt: mt})
		}
		sites = append(sites, site)
	}

	return sites, plain
}

// typeCheck parses name from src into fset and type-checks it alone.
func typeCheck(t *testing.T, fset *token.FileSet, name string, src []byte) (*ast.File, *types.Info) {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	info := newInfo()
	conf := types.Config{Importer: importer.Default()}
	if _, err := conf.Check("fixture", fset, []*ast.File{f}, info); err != nil {
		t.Fatal(err)
	}

	return f, info
}

// newInfo returns a types.Info recording what NewRewriter reads.
func newInfo() *types.Info {
	return &types.Info{
		Types:     map[ast.Expr]types.TypeAndValue{},
		Uses:      map[*ast.Ident]types.Object{},
		Instances: map[*ast.Ident]types.Instance{},
	}
}

// runFixture runs the fixture test binary bin and returns its output.
func runFixture(t *testing.T, bin string, env ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-test.run=^TestPrint$", "-test.timeout=50s")
	cmd.Env = append(os.Environ(), "GREMLINS_MUTANT=", "GREMLINS_REACHED=")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run %s: %v\n%s", bin, err, out)
	}

	return out
}

// buildFixture writes files as a module and compiles its test binary.
func buildFixture(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := writeModule(t, files)
	bin := filepath.Join(dir, "fixture.test")
	if out, err := goCmd(t, dir, nil, "test", "-c", "-o", bin, "."); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	return bin
}

// TestFormsBehave renders every default-mutator site of testdata/forms with
// NewRewriter and requires the one schema binary, run with each mutant id
// (and with none), to print exactly what the plain token mutant -- the source
// with that one operator rewritten, as the engine does -- prints, and to
// record that the site was reached.
func TestFormsBehave(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("testdata/forms/forms.go")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := os.ReadFile("testdata/forms/forms_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, info := typeCheck(t, fset, "forms.go", src)
	df, err := parser.ParseFile(fset, "forms_test.go", driver, 0)
	if err != nil {
		t.Fatal(err)
	}
	sites, plain := discover(f)
	covered := map[mutator.Type]bool{}
	for _, p := range plain {
		covered[p.mt] = true
	}
	if len(covered) != len(formsMutators) {
		t.Fatalf("fixture covers %v, want every one of %v", covered, formsMutators)
	}

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
		"forms.go": out, "forms_test.go": driver, "gremlins_schemata.go": helperFile(t, h, "forms", prefix),
	})
	origBin := buildFixture(t, map[string][]byte{"forms.go": src, "forms_test.go": driver})

	t.Run("id0", func(t *testing.T) {
		t.Parallel()
		reach := filepath.Join(t.TempDir(), "reached")
		want := runFixture(t, origBin)
		// The fixture must exercise what it is there for: recover still
		// catching, and the side-effecting key evaluated exactly once.
		for _, line := range []string{"Recovered: true\n", "IncMap: [map[k:1] 1]\n", "Closures: [0 1 2]\n", "Order: [false 6]\n"} {
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
			to, ok := engine.TokenMutation(p.mt, p.tok)
			if !ok {
				t.Fatalf("engine has no %s mutation for %s", p.mt, p.tok)
			}
			off := fset.Position(p.pos).Offset
			mutated := slices.Concat(src[:off:off], []byte(to.String()), src[off+len(p.tok.String()):])
			want := runFixture(t, buildFixture(t, map[string][]byte{"forms.go": mutated, "forms_test.go": driver}))
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

// TestNewRewriter checks the replacement text of each form, and that every
// shape outside the forms is refused with ErrUnsupported rather than
// rewritten into something that might mean something else.
func TestNewRewriter(t *testing.T) {
	t.Parallel()
	ab := mutator.ArithmeticBase
	cb := mutator.ConditionalsBoundary
	cn := mutator.ConditionalsNegation
	id := mutator.IncrementDecrement
	in := mutator.InvertNegatives
	cases := map[string]struct {
		src     string // a file body after "package p"
		tok     token.Token
		nth     int // which node with tok, in source order
		siteTok token.Token
		muts    []mutator.Type
		want    string // empty: want ErrUnsupported
		// bareInfo drops info.Uses, which the float witness form needs.
		bareInfo bool
		// noFiles gives NewRewriter no files to find a site's context in.
		noFiles bool
	}{
		"eql":             {src: "func f(a, b int) bool { return a == b }", tok: token.EQL, muts: []mutator.Type{cn}, want: "_zzXor(1, a == b)"},
		"neq_iface":       {src: "func f(e error) bool { return e != nil }", tok: token.NEQ, muts: []mutator.Type{cn}, want: "_zzXor(1, e != nil)"},
		"lss_both":        {src: "func f(a, b int) bool { return a < /* c */ b }", tok: token.LSS, muts: []mutator.Type{cb, cn}, want: "_zzLSS(1, 2, a, /* c */ b)"},
		"geq_negation":    {src: "func f(a, b int) bool { return a >= b }", tok: token.GEQ, muts: []mutator.Type{cn}, want: "_zzGEQ(0, 1, a, b)"},
		"gtr_boundary":    {src: "func f(a, b int) bool { return a > b }", tok: token.GTR, muts: []mutator.Type{cb}, want: "_zzGTR(1, 0, a, b)"},
		"leq_untyped":     {src: "type c int\nfunc f(a c) bool { return a <= 3 }", tok: token.LEQ, muts: []mutator.Type{cn, cb}, want: "_zzLEQ(2, 1, a, 3)"},
		"if_condition":    {src: "func f(a, b int) int { if a < b { return 1 }; return 0 }", tok: token.LSS, muts: []mutator.Type{cn}, want: "_zzLSS(0, 1, a, b)"},
		"sub":             {src: "func f(a, b int) int { return a - b }", tok: token.SUB, muts: []mutator.Type{in, ab}, want: "_zzSUB(2, 1, a, b)"},
		"add_multiline":   {src: "func f(a, b int) int {\n\treturn a +\n\t\tb\n}", tok: token.ADD, muts: []mutator.Type{ab}, want: "_zzADD(1, a,\n\t\tb)"},
		"mul_paren":       {src: "func f(a, b, c int) int { return (a + b) * c }", tok: token.MUL, muts: []mutator.Type{ab}, want: "_zzMUL(1, (a + b), c)"},
		"quo":             {src: "func f(a, b float64) float64 { return a / b }", tok: token.QUO, muts: []mutator.Type{ab}, want: "_zzQUO(1, a, b)"},
		"rem":             {src: "func f(a, b uint) uint { return a % b }", tok: token.REM, muts: []mutator.Type{ab}, want: "_zzREM(1, a, b)"},
		"neg":             {src: "func f(x int) int { return -x }", tok: token.SUB, muts: []mutator.Type{ab, in}, want: "_zzNEG(1, 2, x)"},
		"neg_unsigned":    {src: "func f(x uint) uint { return - x }", tok: token.SUB, muts: []mutator.Type{in}, want: "_zzNEG(0, 1, x)"},
		"pos":             {src: "func f(x float32) float32 { return +x }", tok: token.ADD, muts: []mutator.Type{ab}, want: "_zzPOS(1, x)"},
		"inc_ident":       {src: "func f(n int) { n++ }", tok: token.INC, muts: []mutator.Type{id}, want: "_zzIncDec(1, &n, true)"},
		"dec_selector":    {src: "type b struct{ n int8 }\nfunc f(x *b) { x.n-- }", tok: token.DEC, muts: []mutator.Type{id}, want: "_zzIncDec(1, &x.n, false)"},
		"inc_slice":       {src: "func f(s []int, i int) { s[i]++ }", tok: token.INC, muts: []mutator.Type{id}, want: "_zzIncDec(1, &s[i], true)"},
		"inc_deref":       {src: "func f(p *float64) { *p++ }", tok: token.INC, muts: []mutator.Type{id}, want: "_zzIncDec(1, &*p, true)"},
		"inc_for_post":    {src: "func f() { for i := 0; i < 3; i++ {} }", tok: token.INC, muts: []mutator.Type{id}, want: "_zzIncDec(1, &i, true)"},
		"inc_map":         {src: "func f(m map[string]int, k string) { m[k]++ }", tok: token.INC, muts: []mutator.Type{id}, want: "_zzIncDecMap(1, m, k, true)"},
		"dec_map_paren":   {src: "type M map[int]uint\nfunc f(m M) { (m[1])-- }", tok: token.DEC, muts: []mutator.Type{id}, want: "_zzIncDecMap(1, m, 1, false)"},
		"inc_generic_s":   {src: "func f[S ~[]E, E ~int](s S) { s[0]++ }", tok: token.INC, muts: []mutator.Type{id}, want: "_zzIncDec(1, &s[0], true)"},
		"string_add":      {src: "func f(a, b string) string { return a + b }", tok: token.ADD, muts: []mutator.Type{ab}},
		"generic_str_add": {src: "func f[T ~int | ~string](a, b T) T { return a + b }", tok: token.ADD, muts: []mutator.Type{ab}},
		"generic_mul_num": {src: "func f[T ~int | ~float64](a, b T) T { return a * b }", tok: token.MUL, muts: []mutator.Type{ab}, want: "_zzMUL(1, a, b)"},
		// Constant-valued sites: the integer shift form (c/3 folds with
		// integer division), the bool form, the float witness form.
		"const_binary":       {src: "const c = 2\nfunc f() int { return c * 3 }", tok: token.MUL, muts: []mutator.Type{ab}, want: "(6*(1-(1<<_zzBit(1)-1)) + 0*(1<<_zzBit(1)-1))"},
		"const_unary":        {src: "func f(x int) int { return x * -1 }", tok: token.SUB, muts: []mutator.Type{ab, in}, want: "(-1*(1-(1<<_zzBit(1)-1)-(1<<_zzBit(2)-1)) + 1*(1<<_zzBit(1)-1) + 1*(1<<_zzBit(2)-1))"},
		"const_typed":        {src: "type L int\nconst c L = 2\nfunc f() any { return c * 3 }", tok: token.MUL, muts: []mutator.Type{ab}, want: "((c)*0 + 6*(1-(1<<_zzBit(1)-1)) + 0*(1<<_zzBit(1)-1))"},
		"const_rune":         {src: "func f() any { return 'a' + 1 }", tok: token.ADD, muts: []mutator.Type{ab}, want: "(('a')*0 + 98*(1-(1<<_zzBit(1)-1)) + 96*(1<<_zzBit(1)-1))"},
		"const_multiline":    {src: "func f() int {\n\treturn 1 +\n\t\t2\n}", tok: token.ADD, muts: []mutator.Type{ab}, want: "(\n3*(1-(1<<_zzBit(1)-1)) + -1*(1<<_zzBit(1)-1))"},
		"const_float_div":    {src: "func f() int { return 1.5 * 2 }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_compare":      {src: "const c = 2\nfunc f() bool { return c < 3 }", tok: token.LSS, muts: []mutator.Type{cn}, want: "_zzBool1(1, true, false)"},
		"const_bool_if":      {src: "func f() int { if 1 < 2 { return 1 }; return 0 }", tok: token.LSS, muts: []mutator.Type{cb, cn}, want: "_zzBool2(1, 2, true, true, false)"},
		"const_float_call":   {src: "func g(r float64) float64 { return r }\nfunc f() float64 { return g(1.5 * 2) }", tok: token.MUL, muts: []mutator.Type{ab}, want: "_zzSite1(g)"},
		"const_float_method": {src: "type b struct{}\nfunc (b) m(x int, r float32) {}\nfunc f(v b) { v.m(1, (1.5 * 2)) }", tok: token.MUL, muts: []mutator.Type{ab}, want: "_zzSite1(v.m)"},
		"const_float_assign": {src: "func f() float64 { var r float64; r = 1.5 * 2; return r }", tok: token.MUL, muts: []mutator.Type{ab}, want: "_zzSite1(&r)"},
		// Constant-valued sites that stay refused.
		"const_decl":             {src: "const k = 2 * 3", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_decl_local":       {src: "func f() int { const k = 1 + 2; return k }", tok: token.ADD, muts: []mutator.Type{ab}},
		"const_decl_len":         {src: "const k = len([3]int{1 + 1})", tok: token.ADD, muts: []mutator.Type{ab}},
		"const_operand":          {src: "func f() int { return 2*3 + 1 }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_operand_paren":    {src: "func f() int { return (2 * 3) + 1 }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_array_len":        {src: "var a [1 + 1]int", tok: token.ADD, muts: []mutator.Type{ab}},
		"const_array_len_nested": {src: "var a [len([2]int{1 + 1})]int", tok: token.ADD, muts: []mutator.Type{ab}},
		"const_key":              {src: "var s = []int{1 + 1: 5}", tok: token.ADD, muts: []mutator.Type{ab}},
		"const_shift_count":      {src: "func f(x int) int { return x << (1 + 1) }", tok: token.ADD, muts: []mutator.Type{ab}},
		"const_generic_context":  {src: "func f[T ~int](x T) T { return x * (2 + 3) }", tok: token.ADD, muts: []mutator.Type{ab}},
		"const_div_zero":         {src: "func f() int { return 2 * 0 }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_rem":              {src: "func f() int { return 2 % 1 }", tok: token.REM, muts: []mutator.Type{ab}, want: "(0*(1-(1<<_zzBit(1)-1)) + 2*(1<<_zzBit(1)-1))"},
		"const_unrepresentable":  {src: "func f() uint8 { return 255 - 1 }", tok: token.SUB, muts: []mutator.Type{ab}},
		"const_string":           {src: "func f() string { return \"a\" + \"b\" }", tok: token.ADD, muts: []mutator.Type{ab}},
		"const_complex":          {src: "func f() complex128 { return 1i * 2 }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_named_bool":       {src: "type nb bool\nfunc f() nb { return 1 < 2 }", tok: token.LSS, muts: []mutator.Type{cn}},
		"const_foreign_mutator":  {src: "func f() int { return 1 + 2 }", tok: token.ADD, muts: []mutator.Type{mutator.InvertBitwise}},
		"const_float_return":     {src: "func f() float64 { return 1.5 * 2 }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_float_binary":     {src: "func f(x float64) float64 { return x * (1.5 + 1) }", tok: token.ADD, muts: []mutator.Type{ab}},
		"const_float_variadic":   {src: "func g(r ...float64) {}\nfunc f() { g(1.5 * 2) }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_float_generic":    {src: "func g[T ~float64](r T) T { return r }\nfunc f() float64 { return g(1.5 * 2) }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_float_builtin":    {src: "func f(s []float64) []float64 { return append(s, 1.5*2) }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_float_call_fun":   {src: "func g() func(float64) { return nil }\nfunc f() { g()(1.5 * 2) }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_float_arity":      {src: "func g(a, b, c, d, e float64) {}\nfunc f() { g(1, 2, 3, 4, 1.5*2) }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_float_iface":      {src: "func g(v any) {}\nfunc f() { g(1.5 * 2) }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_float_no_uses":    {src: "func g(r float64) {}\nfunc f() { g(1.5 * 2) }", tok: token.MUL, muts: []mutator.Type{ab}, bareInfo: true},
		"const_no_files":         {src: "func f() int { return 1 + 2 }", tok: token.ADD, muts: []mutator.Type{ab}, noFiles: true},
		"const_float_define":     {src: "func f() float64 { r := 1.5 * 2; return r }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_float_map":        {src: "func f(m map[int]float64) { m[1] = 1.5 * 2 }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_float_lhs_call":   {src: "func f(s []float64, i func() int) { s[i()] = 1.5 * 2 }", tok: token.MUL, muts: []mutator.Type{ab}},
		"const_float32":          {src: "func g(r float32) {}\nfunc f() { g(1e38 * 2) }", tok: token.MUL, muts: []mutator.Type{ab}, want: "_zzSite1(g)"},
		"const_float32_overflow": {src: "func g(r float32) {}\nfunc f() { g(3e38 / 2) }", tok: token.QUO, muts: []mutator.Type{ab}},
		// A helper returns plain bool, which a named bool context rejects.
		"named_bool_ordered": {src: "type nb bool\nfunc f(a, b int) nb { return a < b }", tok: token.LSS, muts: []mutator.Type{cn}},
		"named_bool_xor":     {src: "type nb bool\nfunc f(a, b int) nb { return a == b }", tok: token.EQL, muts: []mutator.Type{cn}},
		"generic_map_nested": {src: "type MapC interface{ ~map[string]int }\nfunc f[M interface{ MapC }](m M) { m[\"a\"]++ }", tok: token.INC, muts: []mutator.Type{id}},
		"generic_map_inc":    {src: "func f[M ~map[string]int](m M) { m[\"a\"]++ }", tok: token.INC, muts: []mutator.Type{id}},
		"foreign_mutator":    {src: "func f(a, b int) int { return a + b }", tok: token.ADD, muts: []mutator.Type{mutator.InvertBitwise}},
		"duplicate_mutator":  {src: "func f(a, b int) bool { return a < b }", tok: token.LSS, muts: []mutator.Type{cn, cn}},
		"no_mutants":         {src: "func f(a, b int) bool { return a < b }", tok: token.LSS},
		"tok_mismatch":       {src: "func f(a, b int) bool { return a < b }", tok: token.LSS, siteTok: token.GTR, muts: []mutator.Type{cn}},
		"assign_stmt":        {src: "func f(a int) { a += 1 }", tok: token.ADD_ASSIGN, muts: []mutator.Type{mutator.InvertAssignments}},
		"nested_outer":       {src: "func f(a, b, c int) bool { return a+b < c }", tok: token.LSS, muts: []mutator.Type{cb}, want: "_zzLSS(1, 0, a+b, c)"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := []byte("package p\n\n" + tc.src + "\n")
			fset := token.NewFileSet()
			f, info := typeCheck(t, fset, "p.go", src)
			if tc.bareInfo {
				info.Uses = nil
			}
			var node ast.Node
			seen := 0
			ast.Inspect(f, func(n ast.Node) bool {
				if tn, ok := engine.NewTokenNode(n); ok && tn.Tok() == tc.tok && node == nil {
					if seen == tc.nth {
						node = n
					}
					seen++
				}

				return true
			})
			if node == nil {
				t.Fatalf("no node with %s", tc.tok)
			}
			site := schemata.Site{Node: node, Tok: tc.tok}
			if tc.siteTok != token.ILLEGAL {
				site.Tok = tc.siteTok
			}
			for i, mt := range tc.muts {
				site.Muts = append(site.Muts, schemata.Mutant{ID: i + 1, Type: mt})
			}
			inner := func(n ast.Node) string {
				return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
			}
			files := []*ast.File{f}
			if tc.noFiles {
				files = nil
			}
			got, err := schemata.NewRewriter(info, files, testPrefix, &schemata.HelperSet{})(site, inner)
			if tc.want == "" {
				if !errors.Is(err, schemata.ErrUnsupported) {
					t.Errorf("got %q, %v; want ErrUnsupported", got, err)
				}

				return
			}
			if err != nil || got != tc.want {
				t.Errorf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
