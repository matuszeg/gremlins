// Package red has one function whose only covering tests cannot tell a
// mutant from the original: its honest verdict is LIVED. One of those tests
// fails wherever gremlins runs it, as a test that needs something the
// gremlins copy lacks does (#1397 in matuszeg/plumage).
package red

// Sum is called, never checked.
func Sum(a, b int) int { return a + b }

// Mul is pinned by TestMul alone, which never runs TestBroken; its package
// is red all the same, so its mutant reaches no verdict either.
func Mul(a, b int) int { return a * b }
