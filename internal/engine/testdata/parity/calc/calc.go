// Package calc holds the sites the schemata parity run compares: each of the
// five default mutators, a suppressed site, an uncovered function and a
// mutant that outlives the test timeout.
package calc

import "time"

// Add is pinned by TestAdd: its ARITHMETIC_BASE mutant is killed.
func Add(a, b int) int { return a + b }

// Scale is checked only at b == 1, where * and / agree: its mutant lives.
func Scale(a, b int) int { return a * b }

// Below is checked on both sides of the boundary: both its mutants are killed.
func Below(a, b int) bool { return a < b }

// Pause sleeps when d is at least 1. The test passes 0, so the boundary
// mutant (d > 1) lives and the negated one (d < 1) outlives the timeout.
func Pause(d int) {
	if d >= 1 {
		time.Sleep(time.Hour)
	}
}

// Inc is pinned by TestInc.
func Inc(x int) int {
	x++

	return x
}

// Neg is pinned by TestNeg.
func Neg(x int) int { return -x }

// Diff is covered, but its operator is suppressed: its mutants are skipped.
func Diff(a, b int) int { return a - b } //nomutant

// Unused is called by no test: its mutant is not covered.
func Unused(a, b int) int { return a * b }
