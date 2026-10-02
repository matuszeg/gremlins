package calc

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Error("Add(2, 3) != 5")
	}
}

func TestScale(t *testing.T) {
	if Scale(2, 1) != 2 {
		t.Error("Scale(2, 1) != 2")
	}
}

func TestPause(t *testing.T) {
	Pause(0)
}

func TestInc(t *testing.T) {
	if Inc(1) != 2 {
		t.Error("Inc(1) != 2")
	}
}

func TestNeg(t *testing.T) {
	if Neg(1) != -1 {
		t.Error("Neg(1) != -1")
	}
}
