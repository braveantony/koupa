package sse

import (
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

type ev struct{ Type, Data, ID string }

func parseAll(in string) []ev {
	var out []ev
	Parse(strings.NewReader(in), func(e Event) { out = append(out, ev{e.Type, e.Data, e.ID}) })
	return out
}

// Examples taken from the WHATWG "Server-sent events" section.
func TestParseSpecExamples(t *testing.T) {
	cases := []struct {
		name, in string
		want     []ev
	}{
		{"multi-line data", "data: YHOO\ndata: +2\ndata: 10\n\n", []ev{{"message", "YHOO\n+2\n10", ""}}},
		{"comment, ids, leading space",
			": test stream\n\ndata: first event\nid: 1\n\ndata:second event\nid\n\ndata:  third event\n\n",
			[]ev{{"message", "first event", "1"}, {"message", "second event", ""}, {"message", " third event", ""}}},
		{"empty then two empty lines then unterminated", "data\n\ndata\ndata\n\ndata:",
			[]ev{{"message", "", ""}, {"message", "\n", ""}}},
		{"one leading space only stripped", "data:test\n\ndata: test\n\n",
			[]ev{{"message", "test", ""}, {"message", "test", ""}}},
		{"CRLF", "data: a\r\n\r\ndata: b\r\n\r\n", []ev{{"message", "a", ""}, {"message", "b", ""}}},
		{"CR only", "data: a\r\rdata: b\r\r", []ev{{"message", "a", ""}, {"message", "b", ""}}},
		{"BOM", "\xef\xbb\xbfdata: a\n\n", []ev{{"message", "a", ""}}},
		{"event type", "event: ready\ndata: x\n\ndata: y\n\n", []ev{{"ready", "x", ""}, {"message", "y", ""}}},
		{"id persists", "id: 4\ndata: a\n\ndata: b\n\n", []ev{{"message", "a", "4"}, {"message", "b", "4"}}},
		{"id with NUL ignored", "id: 4\ndata: a\n\nid: 5\x006\ndata: b\n\n", []ev{{"message", "a", "4"}, {"message", "b", "4"}}},
		{"unknown field ignored", "foo: bar\ndata: a\n\n", []ev{{"message", "a", ""}}},
	}
	for _, c := range cases {
		if got := parseAll(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestCRLFSplitAcrossReads(t *testing.T) {
	// A CR at the end of one Read and its LF at the start of the next must be one line ending.
	r := &chunkReader{chunks: []string{"data: a\r", "\n\r", "\ndata: b\n\n"}}
	var out []string
	Parse(r, func(e Event) { out = append(out, e.Data) })
	if !reflect.DeepEqual(out, []string{"a", "b"}) {
		t.Fatalf("got %q", out)
	}
}

type chunkReader struct{ chunks []string }

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[0])
	c.chunks = c.chunks[1:]
	return n, nil
}

func TestEmptyDataDispatches(t *testing.T) {
	got := parseAll("event: shutdown\ndata:\n\n")
	if !reflect.DeepEqual(got, []ev{{"shutdown", "", ""}}) {
		t.Fatalf("got %q", got)
	}
}

func TestNoDataNotDispatched(t *testing.T) {
	// No data line: nothing is dispatched, the event type buffer is reset,
	// but the id still updates the last event ID.
	var p Parser
	var out []ev
	p.Parse(strings.NewReader("event: x\nid: 9\n\n: heartbeat\n\ndata: a\n\n"), func(e Event) { out = append(out, ev{e.Type, e.Data, e.ID}) })
	if !reflect.DeepEqual(out, []ev{{"message", "a", "9"}}) {
		t.Fatalf("got %q", out)
	}
	if p.LastEventID != "9" {
		t.Fatalf("LastEventID=%q", p.LastEventID)
	}
}

func TestRetryUpdatesDelay(t *testing.T) {
	var got []time.Duration
	p := Parser{OnRetry: func(d time.Duration) { got = append(got, d) }}
	p.Parse(strings.NewReader("retry: 1500\n\nretry: 15a\nretry:\nretry: -3\nretry: 20\n\n"), func(Event) {})
	if !reflect.DeepEqual(got, []time.Duration{1500 * time.Millisecond, 20 * time.Millisecond}) {
		t.Fatalf("got %v", got)
	}
}
