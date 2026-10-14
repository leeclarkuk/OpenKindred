package limitio_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/leeclarkuk/openkindred/runtime/internal/limitio"
)

func TestReadAll(t *testing.T) {
	if b, err := limitio.ReadAll(strings.NewReader("abcd"), 4); err != nil || string(b) != "abcd" {
		t.Fatalf("exact fit: %q %v", b, err)
	}
	if _, err := limitio.ReadAll(strings.NewReader("abcde"), 4); !errors.Is(err, limitio.ErrTooLarge) {
		t.Fatalf("one over: %v", err)
	}
}

func TestReader(t *testing.T) {
	if _, err := io.ReadAll(limitio.NewReader(strings.NewReader("abcd"), 4)); err != nil {
		t.Fatalf("exact fit: %v", err)
	}
	if _, err := io.ReadAll(limitio.NewReader(strings.NewReader(strings.Repeat("a", 100)), 10)); !errors.Is(err, limitio.ErrTooLarge) {
		t.Fatalf("over: %v", err)
	}
}
