package calc

// Limit is a package-level constant: its mutant is NOT COVERED, decided
// before any test runs.
const Limit = 4 + 4

// Offset adds a constant expression at run time: 2*3 takes a constant form.
func Offset(x int) int { return x + 2*3 }

// Scaled multiplies by a function-local constant: its declaration is a
// compile-time site, placed by duplicating Scaled.
func Scaled(x int) int {
	const k = 3 - 1

	return x * k
}

// Slots counts the elements of an array whose length is a function-local
// constant: placed by duplicating Slots.
func Slots() int {
	const n = 2 + 1
	var a [n]bool

	return len(a)
}
