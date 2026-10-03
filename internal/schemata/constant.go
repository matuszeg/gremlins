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

package schemata

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/token"
	"go/types"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/go-gremlins/gremlins/internal/mutator"
)

// constMutations mirrors the engine's rewrite table for the operators of the
// default mutators' expression sites: a constant site's mutated values are
// folded from it, so it must rewrite exactly as the engine does (the tests
// hold it to engine.TokenMutation).
var constMutations = map[mutator.Type]map[token.Token]token.Token{
	mutator.ArithmeticBase: {
		token.ADD: token.SUB, token.SUB: token.ADD, token.MUL: token.QUO,
		token.QUO: token.MUL, token.REM: token.MUL,
	},
	mutator.ConditionalsBoundary: {
		token.GEQ: token.GTR, token.GTR: token.GEQ, token.LEQ: token.LSS, token.LSS: token.LEQ,
	},
	mutator.ConditionalsNegation: {
		token.EQL: token.NEQ, token.NEQ: token.EQL, token.GEQ: token.LSS,
		token.GTR: token.LEQ, token.LEQ: token.GTR, token.LSS: token.GEQ,
	},
	mutator.InvertNegatives: {token.SUB: token.ADD},
}

func constMutation(mt mutator.Type, tok token.Token) (token.Token, bool) {
	to, ok := constMutations[mt][tok]

	return to, ok
}

// maxWitnessArity bounds the parameters, and the results, of a callee used
// as a float site's witness.
const maxWitnessArity = 4

// constant rewrites the constant-valued site e. A constant is folded at
// compile time and may be needed there, so it is refused where Go requires
// one -- anywhere in a const declaration, an array length, a composite
// literal key -- and where it is itself an operand of a larger constant
// expression, which keeps the site's mutants out of the outer folding. The
// replacement keeps the line count of the text it replaces.
func (r *rewriter) constant(s Site, e ast.Expr, inner func(ast.Node) string) (string, error) {
	ctx, err := r.context(e)
	if err != nil {
		return "", err
	}
	out, err := constantForm(r.info, e, s, r.prefix, r.h, ctx, inner)
	if err != nil {
		return "", err
	}
	// The replacement is one line; the original's line breaks go after its
	// opening parenthesis, where they cannot end a statement.
	if n := strings.Count(inner(e), "\n"); n > 0 {
		i := strings.Index(out, "(") + 1
		out = out[:i] + strings.Repeat("\n", n) + out[i:]
	}

	return out, nil
}

// context returns e's context: its nearest ancestor that is not a
// parenthesis. It refuses e inside a const declaration or an array length,
// at any depth: anything there must stay constant.
func (r *rewriter) context(e ast.Expr) (ast.Node, error) {
	if r.parents == nil {
		r.parents = map[ast.Node]ast.Node{}
		for _, f := range r.files {
			var stack []ast.Node
			ast.Inspect(f, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]

					return false
				}
				if len(stack) > 0 {
					r.parents[n] = stack[len(stack)-1]
				}
				stack = append(stack, n)

				return true
			})
		}
	}
	if _, ok := r.parents[e]; !ok {
		return nil, fmt.Errorf("%w: constant site outside the package's files", ErrUnsupported)
	}
	var ctx ast.Node
	for child, n := ast.Node(e), r.parents[e]; n != nil; child, n = n, r.parents[n] {
		if _, paren := n.(*ast.ParenExpr); ctx == nil && !paren {
			ctx = n
		}
		switch n := n.(type) {
		case *ast.GenDecl:
			if n.Tok == token.CONST {
				return nil, fmt.Errorf("%w: constant site in a const declaration", ErrUnsupported)
			}
		case *ast.ArrayType:
			if n.Len == child {
				return nil, fmt.Errorf("%w: constant site in an array length", ErrUnsupported)
			}
		}
	}

	return ctx, nil
}

// constantForm returns the replacement for the constant-valued site e, whose
// context -- nearest non-parenthesis ancestor -- is ctx. The original value
// c0 and each mutant's value cK are folded exactly, as the compiler folds
// them, with the mutated operator; which form spells them depends on the
// type the constant takes in its context:
//
//   - an integer type: the shift form, a non-constant expression of
//     untyped constants whose type the context gives, as it gave the
//     constant's: (c0*(1-(1<<PBit(id1)-1)-...) + c1*(1<<PBit(id1)-1) + ...).
//     A constant of a typed operand, or of an untyped rune, adds that
//     operand times 0, so that an interface or inference context still sees
//     the constant's type.
//   - basic bool: PBool<n>(id1, ..., c0, c1, ...), a helper per arity.
//   - float32 or float64, as a call argument or the right side of an
//     assignment: PSite<id>(witness), a helper per site (named after its
//     first mutant) returning T(c0) or
//     T(cK) for the T it infers from the witness -- the callee, or &lhs --
//     which is how a type only another package can name is reached.
//
// Anything else is refused with ErrUnsupported, as is a site whose mutant
// cannot be folded (a division by zero) or does not fit the type.
func constantForm(info *types.Info, e ast.Expr, s Site, prefix string, h *HelperSet, ctx ast.Node, inner func(ast.Node) string) (string, error) {
	if x, ok := ctx.(ast.Expr); ok && info.Types[x].Value != nil {
		return "", fmt.Errorf("%w: operand of a constant expression", ErrUnsupported)
	}
	if kv, ok := ctx.(*ast.KeyValueExpr); ok && ast.Unparen(kv.Key) == e {
		return "", fmt.Errorf("%w: constant composite literal key", ErrUnsupported)
	}
	// In a conversion T(c) the shift form's untyped 1 takes the type T, which
	// for a type parameter is not an integer type the shift accepts, whatever
	// type go/types records for c.
	if call, ok := ctx.(*ast.CallExpr); ok && info.Types[call.Fun].IsType() {
		if _, basic := info.Types[call.Fun].Type.Underlying().(*types.Basic); !basic {
			return "", fmt.Errorf("%w: constant converted to %v", ErrUnsupported, info.Types[call.Fun].Type)
		}
	}
	c, err := foldSite(info, e, s)
	if err != nil {
		return "", err
	}
	t := info.Types[e].Type
	b, _ := t.Underlying().(*types.Basic)
	switch {
	case b == nil || b.Info()&types.IsUntyped != 0 && b.Kind() != types.UntypedBool:
		return "", fmt.Errorf("%w: constant of type %v", ErrUnsupported, t)
	case b.Info()&types.IsBoolean != 0:
		return boolForm(info, e, c, prefix, h)
	case b.Info()&types.IsInteger != 0:
		return intForm(info, e, c, b, prefix, h, inner)
	case b.Kind() == types.Float32 || b.Kind() == types.Float64:
		return floatForm(info, e, c, b, prefix, h, ctx, inner)
	}

	return "", fmt.Errorf("%w: constant of type %v", ErrUnsupported, t)
}

// folded is a constant site's original value and its mutants' values, with
// their ids, in the order of the site's mutator types.
type folded struct {
	c0  constant.Value
	ids []int
	cs  []constant.Value
}

// foldSite folds the site's original value and each present mutant's.
func foldSite(info *types.Info, e ast.Expr, s Site) (folded, error) {
	var mts []mutator.Type
	var op token.Token
	switch e := e.(type) {
	case *ast.BinaryExpr:
		op = e.Op
		switch op { //nolint:exhaustive // only the default mutators' operators fold; default refuses the rest
		case token.ADD, token.MUL, token.QUO, token.REM:
			mts = []mutator.Type{mutator.ArithmeticBase}
		case token.SUB:
			mts = []mutator.Type{mutator.ArithmeticBase, mutator.InvertNegatives}
		case token.LSS, token.LEQ, token.GTR, token.GEQ:
			mts = []mutator.Type{mutator.ConditionalsBoundary, mutator.ConditionalsNegation}
		case token.EQL, token.NEQ:
			mts = []mutator.Type{mutator.ConditionalsNegation}
		}
	case *ast.UnaryExpr:
		op = e.Op
		switch op { //nolint:exhaustive // only - and + are unary sites
		case token.SUB:
			mts = []mutator.Type{mutator.ArithmeticBase, mutator.InvertNegatives}
		case token.ADD:
			mts = []mutator.Type{mutator.ArithmeticBase}
		}
	}
	if mts == nil {
		return folded{}, fmt.Errorf("%w: constant %s site", ErrUnsupported, op)
	}
	all, err := ids(s, mts...)
	if err != nil {
		return folded{}, err
	}
	out := folded{c0: info.Types[e].Value}
	for i, id := range all {
		if id == 0 {
			continue
		}
		to, ok := constMutation(mts[i], op)
		if !ok {
			return folded{}, fmt.Errorf("%w: no %s mutation of %s", ErrUnsupported, mts[i], op)
		}
		v, err := fold(info, e, to)
		if err != nil {
			return folded{}, err
		}
		out.ids = append(out.ids, id)
		out.cs = append(out.cs, v)
	}

	return out, nil
}

// fold evaluates e with its operator replaced by op, as go/types folds a
// constant expression: exactly, with integer division when the operands'
// type is an integer type.
func fold(info *types.Info, e ast.Expr, op token.Token) (constant.Value, error) {
	switch e := e.(type) {
	case *ast.UnaryExpr:
		x := info.Types[e.X].Value
		if x == nil || x.Kind() != constant.Int && x.Kind() != constant.Float {
			return nil, fmt.Errorf("%w: unary operand %v", ErrUnsupported, x)
		}

		return constant.UnaryOp(op, x, 0), nil
	case *ast.BinaryExpr:
		x, y := info.Types[e.X].Value, info.Types[e.Y].Value
		if x == nil || y == nil {
			return nil, fmt.Errorf("%w: operand without a constant value", ErrUnsupported)
		}
		if op.IsOperator() && op.Precedence() == token.EQL.Precedence() {
			return constant.MakeBool(constant.Compare(x, op, y)), nil
		}
		if !numeric(x) || !numeric(y) {
			return nil, fmt.Errorf("%w: operands %v and %v", ErrUnsupported, x, y)
		}
		if (op == token.QUO || op == token.REM) && constant.Sign(y) == 0 {
			return nil, fmt.Errorf("%w: mutant divides by zero", ErrUnsupported)
		}
		if op == token.QUO && integerOperation(info, e) {
			x, y = constant.ToInt(x), constant.ToInt(y)
			if x.Kind() != constant.Int || y.Kind() != constant.Int {
				return nil, fmt.Errorf("%w: integer division of non-integers", ErrUnsupported)
			}
			op = token.QUO_ASSIGN // go/constant's integer division
		}
		if op == token.REM && (x.Kind() != constant.Int || y.Kind() != constant.Int) {
			return nil, fmt.Errorf("%w: remainder of non-integers", ErrUnsupported)
		}

		return constant.BinaryOp(x, op, y), nil
	}

	return nil, fmt.Errorf("%w: constant %T", ErrUnsupported, e)
}

func numeric(v constant.Value) bool {
	return v.Kind() == constant.Int || v.Kind() == constant.Float
}

// integerOperation reports whether the binary e operates in an integer
// type: its typed operand's, or, both untyped, the larger untyped kind.
func integerOperation(info *types.Info, e *ast.BinaryExpr) bool {
	t := operationOperand(info, e)
	b, ok := info.Types[t].Type.Underlying().(*types.Basic)

	return ok && b.Info()&types.IsInteger != 0
}

// operationOperand returns the operand of e whose type the operation has:
// the typed one, or, both untyped, the one of larger kind (int < rune <
// float < complex, which is the order of the untyped basic kinds).
func operationOperand(info *types.Info, e ast.Expr) ast.Expr {
	switch e := e.(type) {
	case *ast.UnaryExpr:
		return e.X
	case *ast.BinaryExpr:
		lt, rt := info.Types[e.X].Type, info.Types[e.Y].Type
		switch {
		case !untyped(lt):
			return e.X
		case !untyped(rt):
			return e.Y
		case basicKind(rt) > basicKind(lt):
			return e.Y
		}

		return e.X
	}

	return e
}

// basicKind is t's kind if t is a basic type, else Invalid.
func basicKind(t types.Type) types.BasicKind {
	if b, ok := t.(*types.Basic); ok {
		return b.Kind()
	}

	return types.Invalid
}

// intForm is the shift form. Each value must fit the context's type b.
func intForm(info *types.Info, e ast.Expr, c folded, b *types.Basic, prefix string, h *HelperSet, inner func(ast.Node) string) (string, error) {
	vals := append([]constant.Value{c.c0}, c.cs...)
	lits := make([]string, len(vals))
	for i, v := range vals {
		v = constant.ToInt(v)
		if v.Kind() != constant.Int || !fitsInt(v, b) {
			return "", fmt.Errorf("%w: value %v does not fit %v", ErrUnsupported, vals[i], b)
		}
		lits[i] = v.ExactString()
	}
	h.Use("Bit")
	var sb strings.Builder
	sb.WriteString("(")
	// The constant's type, where the spelling would lose it: a typed operand
	// carries it; an untyped rune operand gives its default type.
	if op := operationOperand(info, e); basicKind(info.Types[op].Type) != types.UntypedInt {
		sb.WriteString("(" + inner(op) + ")*0 + ")
	}
	sb.WriteString(lits[0] + "*(1")
	for _, id := range c.ids {
		sb.WriteString("-" + bitTerm(prefix, id))
	}
	sb.WriteString(")")
	for i, id := range c.ids {
		sb.WriteString(" + " + lits[i+1] + "*" + bitTerm(prefix, id))
	}
	sb.WriteString(")")

	return sb.String(), nil
}

// bitTerm is 1 when mutant id is active, else 0, in the context's type.
func bitTerm(prefix string, id int) string {
	return "(1<<" + prefix + "Bit(" + strconv.Itoa(id) + ")-1)"
}

// fitsInt reports whether the integer v is representable in b, an integer
// type of the target platform.
func fitsInt(v constant.Value, b *types.Basic) bool {
	sizes := types.SizesFor("gc", build.Default.GOARCH)
	if sizes == nil {
		sizes = types.SizesFor("gc", "amd64")
	}
	bits := uint(64)
	switch sizes.Sizeof(b) {
	case 1:
		bits = 8
	case 2:
		bits = 16
	case 4:
		bits = 32
	}
	lo, hi := constant.MakeInt64(0), constant.Shift(constant.MakeInt64(1), token.SHL, bits)
	if b.Info()&types.IsUnsigned == 0 {
		hi = constant.Shift(constant.MakeInt64(1), token.SHL, bits-1)
		lo = constant.UnaryOp(token.SUB, hi, 0)
	}

	return constant.Compare(v, token.GEQ, lo) && constant.Compare(v, token.LSS, hi)
}

// boolForm calls the helper of the site's arity.
func boolForm(info *types.Info, e ast.Expr, c folded, prefix string, h *HelperSet) (string, error) {
	if err := plainBool(info, e); err != nil {
		return "", err
	}
	n := len(c.ids)
	name := prefix + "Bool" + strconv.Itoa(n)
	var params, args, arms strings.Builder
	for i, id := range c.ids {
		k := strconv.Itoa(i + 1)
		params.WriteString("id" + k + ", ")
		args.WriteString(strconv.Itoa(id) + ", ")
		fmt.Fprintf(&arms, "\tcase %[1]sActive != 0 && %[1]sActive == id%[2]s:\n\t\t%[1]sReached()\n\t\treturn c%[2]s\n", prefix, k)
	}
	cs := []string{"c0"}
	vals := []string{c.c0.ExactString()}
	for i, v := range c.cs {
		cs = append(cs, "c"+strconv.Itoa(i+1))
		vals = append(vals, v.ExactString())
	}
	h.AddRaw(fmt.Sprintf("// %[1]s is a constant comparison's value: c0, or cK while mutant idK is active.\n"+
		"func %[1]s(%[2]sint, %[3]s bool) bool {\n\tswitch {\n%[4]s\t}\n\treturn c0\n}",
		name, strings.TrimSuffix(params.String(), ", ")+" ", strings.Join(cs, ", "), arms.String()))

	return name + "(" + args.String() + strings.Join(vals, ", ") + ")", nil
}

// floatForm is the witness form, in a call argument or assignment context.
func floatForm(info *types.Info, e ast.Expr, c folded, b *types.Basic, prefix string, h *HelperSet, ctx ast.Node, inner func(ast.Node) string) (string, error) {
	vals := append([]constant.Value{c.c0}, c.cs...)
	lits := make([]string, len(vals))
	for i, v := range vals {
		lit, err := floatLit(v, b)
		if err != nil {
			return "", err
		}
		lits[i] = lit
	}
	name := prefix + "Site" + strconv.Itoa(c.ids[0])
	core := "~" + b.Name()
	var typeParams, witnessType, witness string
	switch ctx := ctx.(type) {
	case *ast.CallExpr:
		sig, err := witnessCallee(info, e, ctx, b)
		if err != nil {
			return "", err
		}
		i := slices.IndexFunc(ctx.Args, func(a ast.Expr) bool { return ast.Unparen(a) == e })
		var tps, ps, rs []string
		for j := range sig.Params().Len() {
			if j == i {
				ps = append(ps, "T")

				continue
			}
			tps = append(tps, "A"+strconv.Itoa(j))
			ps = append(ps, "A"+strconv.Itoa(j))
		}
		for j := range sig.Results().Len() {
			tps = append(tps, "R"+strconv.Itoa(j))
			rs = append(rs, "R"+strconv.Itoa(j))
		}
		typeParams = "T " + core
		if len(tps) > 0 {
			typeParams += ", " + strings.Join(tps, ", ") + " any"
		}
		witnessType = "func(" + strings.Join(ps, ", ") + ")"
		if len(rs) > 0 {
			witnessType += " (" + strings.Join(rs, ", ") + ")"
		}
		witness = inner(ctx.Fun)
	case *ast.AssignStmt:
		lhs, err := witnessLHS(info, e, ctx, b)
		if err != nil {
			return "", err
		}
		typeParams, witnessType, witness = "T "+core, "*T", "&"+inner(lhs)
	default:
		return "", fmt.Errorf("%w: float constant in a %T", ErrUnsupported, ctx)
	}
	var arms strings.Builder
	for i, id := range c.ids {
		fmt.Fprintf(&arms, "\tif %[1]sActive != 0 && %[1]sActive == %[2]d {\n\t\t%[1]sReached()\n\t\treturn T(%[3]s)\n\t}\n", prefix, id, lits[i+1])
	}
	h.AddRaw(fmt.Sprintf("// %[1]s is a float constant's value, in the type T inferred from the witness.\n"+
		"func %[1]s[%[2]s](%[3]s) T {\n%[4]s\treturn T(%[5]s)\n}",
		name, typeParams, witnessType, arms.String(), lits[0]))

	return name + "(" + witness + ")", nil
}

// floatLit spells v as an untyped constant expression of its exact value,
// refusing a value that overflows b.
func floatLit(v constant.Value, b *types.Basic) (string, error) {
	v = constant.ToFloat(v)
	f, _ := constant.Float64Val(v)
	if b.Kind() == types.Float32 {
		f32, _ := constant.Float32Val(v)
		f = float64(f32)
	}
	num, den := constant.Num(v), constant.Denom(v)
	if math.IsInf(f, 0) || num.Kind() != constant.Int || den.Kind() != constant.Int {
		return "", fmt.Errorf("%w: value %v does not fit %v", ErrUnsupported, v, b)
	}
	if constant.Compare(den, token.EQL, constant.MakeInt64(1)) {
		return num.ExactString(), nil
	}

	return num.ExactString() + " / " + den.ExactString() + ".0", nil
}

// witnessCallee returns the signature of the call ctx that e is an argument
// of, if its callee can be passed as the witness: evaluated twice without
// effect (identifiers and selectors only), a function or variable that is
// not generic, with at most maxWitnessArity parameters and results, e not
// variadic, and e's parameter of the type b the constant took.
func witnessCallee(info *types.Info, e ast.Expr, call *ast.CallExpr, b *types.Basic) (*types.Signature, error) {
	i := slices.IndexFunc(call.Args, func(a ast.Expr) bool { return ast.Unparen(a) == e })
	if i < 0 || call.Ellipsis.IsValid() {
		return nil, fmt.Errorf("%w: float constant not a plain call argument", ErrUnsupported)
	}
	if info.Uses == nil {
		return nil, fmt.Errorf("%w: no Uses to check a float constant's callee", ErrUnsupported)
	}
	id := calleeIdent(info, call.Fun)
	if id == nil {
		return nil, fmt.Errorf("%w: float constant's callee is not a name or a qualified name", ErrUnsupported)
	}
	switch obj := info.Uses[id].(type) {
	case *types.Func:
		if obj.Signature().TypeParams() != nil {
			return nil, fmt.Errorf("%w: float constant's callee is generic", ErrUnsupported)
		}
	case *types.Var:
	default:
		return nil, fmt.Errorf("%w: float constant's callee is a %T", ErrUnsupported, obj)
	}
	sig, ok := info.Types[call.Fun].Type.Underlying().(*types.Signature)
	switch {
	case !ok:
		return nil, fmt.Errorf("%w: float constant's callee has no signature", ErrUnsupported)
	case sig.Variadic() && i >= sig.Params().Len()-1:
		return nil, fmt.Errorf("%w: float constant is a variadic argument", ErrUnsupported)
	case sig.Params().Len() > maxWitnessArity || sig.Results().Len() > maxWitnessArity:
		return nil, fmt.Errorf("%w: float constant's callee has over %d parameters or results", ErrUnsupported, maxWitnessArity)
	case !types.Identical(sig.Params().At(i).Type().Underlying(), b):
		return nil, fmt.Errorf("%w: float constant's parameter is %v", ErrUnsupported, sig.Params().At(i).Type())
	}

	return sig, nil
}

// calleeIdent returns the identifier naming a callee that is a plain or
// package-qualified name, or nil. Evaluating either a second time, as the
// witness, has no effect and cannot panic. A method value or a field
// (x.M, x.f) is refused: a nil x panics when the witness is evaluated, among
// the arguments, where the original panics at the call, after all of them --
// a later argument's effects would be lost.
func calleeIdent(info *types.Info, fun ast.Expr) *ast.Ident {
	switch f := ast.Unparen(fun).(type) {
	case *ast.Ident:
		return f
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			if _, pkg := info.Uses[x].(*types.PkgName); pkg {
				return f.Sel
			}
		}
	}

	return nil
}

// callFree reports whether x is identifiers and selectors only.
func callFree(x ast.Expr) bool {
	switch x := ast.Unparen(x).(type) {
	case *ast.Ident:
		return true
	case *ast.SelectorExpr:
		return callFree(x.X)
	}

	return false
}

// witnessLHS returns the left side assigned e by the single plain assignment
// st, if it can be passed as &lhs: addressable, of the type b the constant
// took, and identifiers and selectors only. The witness is the original
// text, so the lhs must hold no site (no index, no call), whose rendered
// form the assignment would evaluate but the witness would not; and it must
// dereference no pointer, which the witness would do before the site records
// its reach.
func witnessLHS(info *types.Info, e ast.Expr, st *ast.AssignStmt, b *types.Basic) (ast.Expr, error) {
	i := slices.IndexFunc(st.Rhs, func(r ast.Expr) bool { return ast.Unparen(r) == e })
	if st.Tok != token.ASSIGN || i < 0 || len(st.Lhs) != 1 || len(st.Rhs) != 1 {
		return nil, fmt.Errorf("%w: float constant not the value of a plain assignment", ErrUnsupported)
	}
	lhs := st.Lhs[i]
	tv := info.Types[lhs]
	switch {
	case !tv.Addressable():
		return nil, fmt.Errorf("%w: float constant assigned to a non-addressable operand", ErrUnsupported)
	case tv.Type == nil || !types.Identical(tv.Type.Underlying(), b):
		return nil, fmt.Errorf("%w: float constant assigned to a %v", ErrUnsupported, tv.Type)
	case !callFree(lhs):
		return nil, fmt.Errorf("%w: float constant assigned to more than a name or selector", ErrUnsupported)
	case !derefFree(info, lhs):
		return nil, fmt.Errorf("%w: float constant assigned through a pointer", ErrUnsupported)
	}

	return lhs, nil
}

// derefFree reports whether evaluating &x, for x of identifiers and selectors
// only, dereferences no pointer: no field selection in it goes through one,
// implicitly or through an embedded field. A nil pointer there would panic
// among the witness's arguments, before the site records its reach, where
// the original panics at the store, after it. A package-qualified name
// dereferences nothing; a selection the type information does not hold is
// refused.
func derefFree(info *types.Info, x ast.Expr) bool {
	sel, ok := ast.Unparen(x).(*ast.SelectorExpr)
	if !ok {
		return true
	}
	if id, ok := ast.Unparen(sel.X).(*ast.Ident); ok && info.Uses != nil {
		if _, pkg := info.Uses[id].(*types.PkgName); pkg {
			return true
		}
	}
	s, ok := info.Selections[sel]
	if !ok || s.Kind() != types.FieldVal || s.Indirect() {
		return false
	}
	if _, ptr := s.Recv().Underlying().(*types.Pointer); ptr {
		return false
	}

	return derefFree(info, sel.X)
}
