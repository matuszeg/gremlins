package red

import (
	"os"
	"testing"
)

func TestSumRuns(t *testing.T) {
	_ = Sum(1, 2)
}

// TestBroken passes under `go test` and fails under gremlins, which sets
// GOTMPDIR for every test process it starts: red with no mutant at all.
func TestBroken(t *testing.T) {
	_ = Sum(1, 2)
	if os.Getenv("GOTMPDIR") != "" {
		t.Fatal("fails wherever gremlins runs it")
	}
}

func TestMul(t *testing.T) {
	if Mul(2, 3) != 6 {
		t.Fatal("Mul(2, 3) != 6")
	}
}
