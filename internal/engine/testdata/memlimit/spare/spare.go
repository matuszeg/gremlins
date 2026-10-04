// Package spare allocates what is left of a GiB. Its ARITHMETIC_BASE mutant
// allocates nearly two GiB instead and still passes its test: it lives
// without a memory limit and dies under one of a GiB.
package spare

const gib = 1 << 30

// Spare allocates what remains of a GiB after used bytes, and reports
// whether it got any.
func Spare(used int) bool {
	buf := make([]byte, gib-used)

	return len(buf) != 0
}
