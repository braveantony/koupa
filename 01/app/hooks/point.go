// Package hooks defines the test pause points (plan A12). Production uses
// Noop; the TEST_HOOKS implementation is Registry.
//
// A wait reports only pauses whose request is still live, but a request can
// end right after it was reported. Scripts must therefore confirm what
// happened to an attempt through the per-attempt logs (hook_released,
// request_end, SIGTERM lines), not through the wait reply alone.
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
