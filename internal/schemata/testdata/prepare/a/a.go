package a

// Add sums x and y.
func Add(x, y int) int { return x + y }

// Bump adds one to x in place: a site the rewrite does not support.
func Bump(x int) int {
	x += 1

	return x
}
