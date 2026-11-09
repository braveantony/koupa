package applog

import (
	"io"
	"sync"
)

// Gate is a log destination that can be sealed so a chosen line is the
// last one the process writes (the sigterm line in step 0, the exit line
// otherwise), even while other goroutines keep logging.
type Gate struct {
	mu     sync.Mutex
	w      io.Writer
	sealed bool
}

// NewGate wraps w.
func NewGate(w io.Writer) *Gate { return &Gate{w: w} }

// Write passes p through until the gate is sealed, then drops it.
func (g *Gate) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return len(p), nil
	}
	return g.w.Write(p)
}

// Final returns a writer whose first Write goes out and seals the gate in
// one step; nothing written to the gate afterwards appears.
func (g *Gate) Final() io.Writer { return finalWriter{g} }

type finalWriter struct{ g *Gate }

func (f finalWriter) Write(p []byte) (int, error) {
	f.g.mu.Lock()
	defer f.g.mu.Unlock()
	if f.g.sealed {
		return len(p), nil
	}
	f.g.sealed = true
	return f.g.w.Write(p)
}
