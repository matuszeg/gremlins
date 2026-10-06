// Package spare allocates what is left of two GiB. Its ARITHMETIC_BASE mutant
// allocates nearly four GiB instead and still passes its test: it lives
// without a memory limit and dies under one of three GiB.
package spare

const quota = 2 << 30

// Spare allocates what remains of two GiB after used bytes, and reports
// whether it got any.
func Spare(used int) bool {
	buf := make([]byte, quota-used)

	return len(buf) != 0
}
