package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"nope"},
		{"run", "--base", "http://x", "--run", "r1"},
		{"run", "--base", "http://x", "--run", "r1", "--out", "d", "--duration", "10s"},
		{"run", "--base", "http://x", "--run", "r_1", "--out", "d"},
		{"run", "--base", "http://x", "--run", "r1", "--out", "d", "--no-ingress", "--ingress-log", "f"},
		{"probe", "--base", "http://x", "--run", "r1", "--out", "d"},
		{"summarize"},
	} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
		if strings.Contains(out.String(), "SENDING_STARTED") {
			t.Errorf("%v started sending", args)
		}
	}
}
