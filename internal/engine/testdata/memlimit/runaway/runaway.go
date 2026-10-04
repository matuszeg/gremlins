// Package runaway holds a loop whose INCREMENT_DECREMENT mutant never ends
// and allocates a MiB on every pass, the shape that exhausts a machine.
package runaway

const mib = 1 << 20

// Chunks returns n buffers of a MiB each.
func Chunks(n int) [][]byte {
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, make([]byte, mib))
	}

	return out
}
