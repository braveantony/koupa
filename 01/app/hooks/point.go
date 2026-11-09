// Package hooks defines the test pause points (plan A12). Production uses
// Noop; the TEST_HOOKS implementation arrives with the control interface.
package hooks

import "context"

// Point pauses a request at a named moment until released or ctx ends.
// attemptID is the request's X-Koupa-Attempt header, possibly empty.
type Point interface {
	Pause(ctx context.Context, name, attemptID string)
}

// Noop never pauses.
type Noop struct{}

// Pause returns immediately.
func (Noop) Pause(context.Context, string, string) {}
