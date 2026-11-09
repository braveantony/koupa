package main

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestAwaitStartupPrefersSignal(t *testing.T) {
	// Both ready: select alone would pick either at random, so repeat.
	for i := 0; i < 200; i++ {
		openc := make(chan opened, 1)
		sigs := make(chan os.Signal, 1)
		openc <- opened{err: errors.New("db down")}
		sigs <- syscall.SIGTERM
		if _, sig := awaitStartup(openc, sigs); sig != syscall.SIGTERM {
			t.Fatalf("iteration %d: signal not preferred (got %v)", i, sig)
		}
	}
}

func TestAwaitStartupResultOrSignal(t *testing.T) {
	openc := make(chan opened, 1)
	sigs := make(chan os.Signal, 1)
	want := errors.New("db down")
	openc <- opened{err: want}
	if o, sig := awaitStartup(openc, sigs); sig != nil || o.err != want {
		t.Fatalf("result only: %+v %v", o, sig)
	}
	sigs <- syscall.SIGTERM
	if _, sig := awaitStartup(make(chan opened), sigs); sig != syscall.SIGTERM {
		t.Fatalf("signal only: %v", sig)
	}
}
