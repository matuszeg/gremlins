package runaway

import "testing"

func TestChunks(t *testing.T) {
	if got := len(Chunks(2)); got != 2 {
		t.Fatalf("Chunks(2) has %d buffers, want 2", got)
	}
}
