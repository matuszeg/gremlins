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
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"github.com/go-gremlins/gremlins/internal/mutator"
)

// Mirrors of the helper constraints, for checking an operand type against
// them before a call the compiler would reject is emitted.
var (
	integerKinds = []types.BasicKind{
		types.Int, types.Int8, types.Int16, types.Int32, types.Int64,
		types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr,
	}
	integerConstraint = constraint(integerKinds...)
	numberConstraint  = constraint(append(integerKinds[:len(integerKinds):len(integerKinds)],
		types.Float32, types.Float64, types.Complex64, types.Complex128)...)
	orderedConstraint = constraint(append(integerKinds[:len(integerKinds):len(integerKinds)],
		types.Float32, types.Float64, types.String)...)
)

// constraint is the interface ~k1 | ~k2 | ... over basic kinds.
func constraint(kinds ...types.BasicKind) *types.Interface {
	terms := make([]*types.Term, len(kinds))
	for i, k := range kinds {
		terms[i] = types.NewTerm(true, types.Typ[k])
	}
	it := types.NewInterfaceType(nil, []types.Type{types.NewUnion(terms)})
	it.Complete()

	return it
}

// orderedHelpers names the comparison helper of each ordered operator.
var orderedHelpers = map[token.Token]string{
	token.LSS: "LSS", token.LEQ: "LEQ", token.GTR: "GTR", token.GEQ: "GEQ",
}

// arithHelpers names the ARITHMETIC_BASE helper of each binary operator but
// -, which INVERT_NEGATIVES shares and SUB handles.
var arithHelpers = map[token.Token]string{
	token.ADD: "ADD", token.MUL: "MUL", token.QUO: "QUO", token.REM: "REM",
}

// srcRange is a source range that is not itself a node, passed to inner to read
// the text between and around a site's operands.
type srcRange struct{ pos, end token.Pos }

func (s srcRange) Pos() token.Pos { return s.pos }
func (s srcRange) End() token.Pos { return s.end }

// NewRewriter returns the Rewriter for the default mutators' sites:
// comparisons, binary and unary arithmetic, and ++/-- statements. Each site
// becomes one call to a fixed helper prefixed with prefix, recorded in h,
// whose arguments are the site's operands -- each appearing exactly once, in
// their original order, with their original line breaks -- so that with no
// mutant active the program evaluates what it did before, and with one of
// the site's mutants active it evaluates what the engine's token mutant
// would. info must hold the types of the package the sites come from. A site
// outside those forms, or one whose operand types the helper cannot take,
// is refused with ErrUnsupported.
func NewRewriter(info *types.Info, prefix string, h *HelperSet) Rewriter {
	r := &rewriter{info: info, prefix: prefix, h: h}

	return r.rewrite
}

type rewriter struct {
	info   *types.Info
	prefix string
	h      *HelperSet
}

func (r *rewriter) rewrite(s Site, inner func(ast.Node) string) (string, error) {
	if len(s.Muts) == 0 {
		return "", fmt.Errorf("%w: site has no mutants", ErrUnsupported)
	}
	switch n := s.Node.(type) {
	case *ast.BinaryExpr:
		if err := r.checkSite(s, n.Op, n); err != nil {
			return "", err
		}

		return r.binary(s, n, inner)
	case *ast.UnaryExpr:
		if err := r.checkSite(s, n.Op, n); err != nil {
			return "", err
		}

		return r.unary(s, n, inner)
	case *ast.IncDecStmt:
		if err := r.checkSite(s, n.Tok, nil); err != nil {
			return "", err
		}

		return r.incDec(s, n, inner)
	}

	return "", fmt.Errorf("%w: %T site", ErrUnsupported, s.Node)
}

// checkSite refuses a site whose token is not its node's, and an expression
// site whose value is constant.
func (r *rewriter) checkSite(s Site, op token.Token, e ast.Expr) error {
	if s.Tok != op {
		return fmt.Errorf("%w: site token %s is not the node's %s", ErrUnsupported, s.Tok, op)
	}
	if e != nil && r.info.Types[e].Value != nil {
		// A constant-valued site cannot become a call: the value may be
		// needed at compile time, and its operands are folded exactly
		// rather than in the type's arithmetic. This is the hook for Task 5's
		// constantForm.
		return fmt.Errorf("%w: constant-valued %s site", ErrUnsupported, op)
	}

	return nil
}

// ids returns the site's mutant ids in the order of types, 0 for a type
// that is absent. Any mutant of another type, or a repeated type, is refused.
func ids(s Site, mts ...mutator.Type) ([]int, error) {
	out := make([]int, len(mts))
	for _, m := range s.Muts {
		i := -1
		for j, mt := range mts {
			if mt == m.Type {
				i = j
			}
		}
		switch {
		case i < 0:
			return nil, fmt.Errorf("%w: no form for %s at %s", ErrUnsupported, m.Type, s.Tok)
		case out[i] != 0:
			return nil, fmt.Errorf("%w: %s twice at one site", ErrUnsupported, m.Type)
		}
		out[i] = m.ID
	}

	return out, nil
}

// call returns prefix+name(ids..., args...) and records the helper. An
// argument that starts on a new line follows its comma directly.
func (r *rewriter) call(name string, ids []int, args ...string) string {
	r.h.Use(name)
	parts := make([]string, 0, len(ids)+len(args))
	for _, id := range ids {
		parts = append(parts, strconv.Itoa(id))
	}
	parts = append(parts, args...)
	var b strings.Builder
	b.WriteString(r.prefix + name + "(")
	for i, p := range parts {
		switch {
		case i == 0:
		case strings.HasPrefix(p, "\n"):
			b.WriteString(",")
		default:
			b.WriteString(", ")
		}
		b.WriteString(p)
	}
	b.WriteString(")")

	return b.String()
}

func (r *rewriter) binary(s Site, e *ast.BinaryExpr, inner func(ast.Node) string) (string, error) {
	switch e.Op { //nolint:exhaustive // only the default mutators' operators have forms; default refuses the rest
	case token.EQL, token.NEQ:
		id, err := ids(s, mutator.ConditionalsNegation)
		if err != nil {
			return "", err
		}
		if err := r.boolResult(e); err != nil {
			return "", err
		}
		// The comparison stays as written, an argument of its own: no
		// operand types to match, and nothing to re-parenthesise.
		return r.call("Xor", id, inner(e)), nil
	case token.LSS, token.LEQ, token.GTR, token.GEQ:
		id, err := ids(s, mutator.ConditionalsBoundary, mutator.ConditionalsNegation)
		if err != nil {
			return "", err
		}
		if err := r.boolResult(e); err != nil {
			return "", err
		}
		if err := r.operands(e, orderedConstraint); err != nil {
			return "", err
		}

		return r.call(orderedHelpers[e.Op], id, operandText(e, inner)...), nil
	case token.SUB:
		id, err := ids(s, mutator.ArithmeticBase, mutator.InvertNegatives)
		if err != nil {
			return "", err
		}
		if err := r.operands(e, numberConstraint); err != nil {
			return "", err
		}

		return r.call("SUB", id, operandText(e, inner)...), nil
	case token.ADD, token.MUL, token.QUO, token.REM:
		id, err := ids(s, mutator.ArithmeticBase)
		if err != nil {
			return "", err
		}
		c := numberConstraint
		if e.Op == token.REM {
			c = integerConstraint
		}
		if err := r.operands(e, c); err != nil {
			return "", err
		}

		return r.call(arithHelpers[e.Op], id, operandText(e, inner)...), nil
	default:
		return "", fmt.Errorf("%w: binary %s", ErrUnsupported, e.Op)
	}
}

// operandText splits e's text at its operator: the left operand with the
// space before the operator dropped, and the right operand with the space
// after it dropped -- but any line break or comment kept, so the call has
// the lines the expression had.
func operandText(e *ast.BinaryExpr, inner func(ast.Node) string) []string {
	opEnd := e.OpPos + token.Pos(len(e.Op.String()))

	return []string{
		strings.TrimRight(inner(srcRange{e.Pos(), e.OpPos}), " \t"),
		strings.TrimLeft(inner(srcRange{opEnd, e.End()}), " \t"),
	}
}

// boolResult refuses a comparison whose result is used as a named boolean
// type: the helpers return bool, which such a context does not accept.
func (r *rewriter) boolResult(e ast.Expr) error {
	t := r.info.Types[e].Type
	if t == nil || !types.Identical(t, types.Typ[types.Bool]) && !types.Identical(t, types.Typ[types.UntypedBool]) {
		return fmt.Errorf("%w: comparison of type %v, not bool", ErrUnsupported, t)
	}

	return nil
}

// operands refuses a binary site unless both operands have one type, which
// satisfies c, for the helper's single type parameter to be inferred as. An
// untyped constant operand takes the other operand's type, as it did in the
// original expression.
func (r *rewriter) operands(e *ast.BinaryExpr, c *types.Interface) error {
	l, rt := r.info.Types[e.X], r.info.Types[e.Y]
	var t types.Type
	switch {
	case l.Type == nil || rt.Type == nil:
		return fmt.Errorf("%w: operand without type information", ErrUnsupported)
	case !untyped(l.Type) && !untyped(rt.Type):
		if !types.Identical(l.Type, rt.Type) {
			return fmt.Errorf("%w: operand types %v and %v differ", ErrUnsupported, l.Type, rt.Type)
		}
		t = l.Type
	case !untyped(l.Type) && rt.Value != nil:
		t = l.Type
	case !untyped(rt.Type) && l.Value != nil:
		t = rt.Type
	default:
		return fmt.Errorf("%w: untyped operands %v and %v", ErrUnsupported, l.Type, rt.Type)
	}

	return satisfies(t, c)
}

func satisfies(t types.Type, c *types.Interface) error {
	if !types.Satisfies(t, c) {
		return fmt.Errorf("%w: operand type %v outside the helper's constraint %v", ErrUnsupported, t, c)
	}

	return nil
}

func untyped(t types.Type) bool {
	b, ok := t.(*types.Basic)

	return ok && b.Info()&types.IsUntyped != 0
}

func (r *rewriter) unary(s Site, e *ast.UnaryExpr, inner func(ast.Node) string) (string, error) {
	var name string
	var id []int
	var err error
	switch e.Op { //nolint:exhaustive // only - and + are unary sites; default refuses the rest
	case token.SUB:
		name = "NEG"
		id, err = ids(s, mutator.ArithmeticBase, mutator.InvertNegatives)
	case token.ADD:
		name = "POS"
		id, err = ids(s, mutator.ArithmeticBase)
	default:
		return "", fmt.Errorf("%w: unary %s", ErrUnsupported, e.Op)
	}
	if err != nil {
		return "", err
	}
	t := r.info.Types[e.X].Type
	if t == nil || untyped(t) {
		return "", fmt.Errorf("%w: unary operand of type %v", ErrUnsupported, t)
	}
	if err := satisfies(t, numberConstraint); err != nil {
		return "", err
	}
	// The operand with any space after the operator dropped, line breaks and
	// comments kept.
	x := strings.TrimLeft(inner(srcRange{e.OpPos + 1, e.End()}), " \t")

	return r.call(name, id, x), nil
}

// incDec rewrites x++ / x-- as a call statement, legal wherever the
// statement was, a for post statement included. The operand is evaluated
// once, by the call's arguments: &x for an addressable x, or the map and the
// key for a map entry, which is not addressable.
func (r *rewriter) incDec(s Site, st *ast.IncDecStmt, inner func(ast.Node) string) (string, error) {
	id, err := ids(s, mutator.IncrementDecrement)
	if err != nil {
		return "", err
	}
	inc := strconv.FormatBool(st.Tok == token.INC)
	t := r.info.Types[st.X].Type
	if t == nil {
		return "", fmt.Errorf("%w: operand without type information", ErrUnsupported)
	}
	if err := satisfies(t, numberConstraint); err != nil {
		return "", err
	}
	ix, ok := ast.Unparen(st.X).(*ast.IndexExpr)
	if !ok {
		return r.call("IncDec", id, "&"+inner(st.X), inc), nil
	}
	isMap, err := r.mapIndex(ix)
	if err != nil {
		return "", err
	}
	if !isMap {
		return r.call("IncDec", id, "&"+inner(st.X), inc), nil
	}
	m := inner(ix.X)
	k := strings.Trim(inner(srcRange{ix.Lbrack + 1, ix.Rbrack}), " \t")

	return r.call("IncDecMap", id, m, k, inc), nil
}

// mapIndex reports whether ix indexes a map. An operand of type-parameter
// type is a map only if some type in its constraint is; that case is
// refused, as no one helper takes both a map and a slice.
func (r *rewriter) mapIndex(ix *ast.IndexExpr) (bool, error) {
	t := r.info.Types[ix.X].Type
	if t == nil {
		return false, fmt.Errorf("%w: indexed operand without type information", ErrUnsupported)
	}
	tp, ok := t.(*types.TypeParam)
	if !ok {
		_, isMap := t.Underlying().(*types.Map)

		return isMap, nil
	}
	iface, ok := tp.Constraint().Underlying().(*types.Interface)
	if !ok {
		return false, fmt.Errorf("%w: type parameter %v without an interface constraint", ErrUnsupported, tp)
	}
	for i := range iface.NumEmbeddeds() {
		u, ok := iface.EmbeddedType(i).(*types.Union)
		if !ok {
			if _, isMap := iface.EmbeddedType(i).Underlying().(*types.Map); isMap {
				return false, fmt.Errorf("%w: map-typed type parameter %v", ErrUnsupported, tp)
			}

			continue
		}
		for j := range u.Len() {
			if _, isMap := u.Term(j).Type().Underlying().(*types.Map); isMap {
				return false, fmt.Errorf("%w: map-typed type parameter %v", ErrUnsupported, tp)
			}
		}
	}

	return false, nil
}
