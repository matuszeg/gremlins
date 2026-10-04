package spare

import "testing"

func TestSpare(t *testing.T) {
	if !Spare(gib - 1<<20) {
		t.Fatal("Spare left nothing of a GiB after a MiB")
	}
}
