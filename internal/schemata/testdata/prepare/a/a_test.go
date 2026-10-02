package a

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatal("Add(2, 3) != 5")
	}
	if Bump(1) != 2 {
		t.Fatal("Bump(1) != 2")
	}
}
