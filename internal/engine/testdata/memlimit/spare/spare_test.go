package spare

import "testing"

func TestSpare(t *testing.T) {
	if !Spare(quota - 1<<20) {
		t.Fatal("Spare left nothing of two GiB after a MiB")
	}
}
