package app

import (
	"testing"
	"time"
)

func TestJitterStaysWithinTwentyPercent(t *testing.T) {
	d := 10 * time.Second
	for i := 0; i < 1000; i++ {
		if j := jitter(d); j < 8*time.Second || j > 12*time.Second {
			t.Fatalf("jitter(%s) = %s", d, j)
		}
	}
}
