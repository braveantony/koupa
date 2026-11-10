package wire_test

import (
	"testing"

	"koupa/internal/wire"
)

func TestNewAttemptID(t *testing.T) {
	if got := wire.NewAttemptID("r1", 7, 2); got != "r1-a7-t2" {
		t.Fatalf("got %q", got)
	}
}
