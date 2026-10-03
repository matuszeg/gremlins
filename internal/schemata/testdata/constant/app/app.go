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

// Package app is the fixture for TestConstantFormsBehave: constant-valued
// sites, each in a context that decides its form. A line marked
// "refused <op>" holds a site of that operator that must stay
// ErrUnsupported. Every plain token mutant of every site compiles.
package app

import "fixture/foo"

const k = 2 * 3 // refused *: a const declaration

func SetSum() any { return foo.Set(1 + 2) }

func SetParen() any { return foo.Set((4 - 1)) }

func Small() any {
	y := foo.Small()
	y = 200 + 50
	return y
}

func Ratio() any {
	x := foo.SetR(1.5 * 2)
	return x
}

func RatioAssign() any {
	r := foo.R()
	r = 1.5 / 4
	return r
}

func Mix() any {
	l, r := foo.Mix(3, 0.5+0.25)
	return []any{l, r}
}

func Method() any {
	var b foo.Box
	b.Put(3.0 - 0.5) // refused -: a method value witness can panic out of order
	return b.R
}

func Neg() int { return -1 }

func NegOperand(x int) int { return x * -2 }

func Quo() int { return 7 / 2 }

func Typed() any { return foo.Unit * 3 }

func Rune() any { return 'a' + 1 }

func Cmp() bool { return k < 6 }

func CmpEq() bool { return k == 6 }

func If() int {
	if 2 < 2 {
		return 1
	}
	return 0
}

func Nested() int { return 2*3 + 1 } // refused *: 2*3 is an operand of a constant

func MultiLine() int {
	return 10 +
		5
}

func Arr() int {
	var a [1 + 1]int // an array length: placed by duplicating Arr
	return len(a)
}

func FloatBin(f float64) float64 { return f * (1.5 + 1) } // refused +: a float operand

func Key() []int { return []int{1 + 1: 5} } // a composite literal key: placed by duplicating Key

func Shift(x int) int { return x << (1 + 1) } // refused +: an untyped shift count

func Variadic() any { return foo.Sum(0.5 + 1) } // refused +: a variadic float argument

func Generic() any { return foo.Pick(2.5 * 2) } // refused *: a generic callee

func SetAnd() any { return foo.Set(6 & 3) }

func SetShl() any { return foo.Set(1 << 4) }

func Land() bool {
	x := true && false
	return x
}
