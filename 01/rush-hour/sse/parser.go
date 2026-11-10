// Package sse is an EventSource-compatible Server-Sent Events client used by
// Rush Hour to watch the board stream. Parsing follows the WHATWG HTML
// "Server-sent events" section ("Parsing an event stream" and "Interpreting
// an event stream").
package sse

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxRetry is the largest reconnection time accepted from a retry field.
const MaxRetry = time.Hour

// Event is one dispatched event. ID is the last event ID at dispatch time,
// which persists across events until another id field changes it.
type Event struct {
	Type string // "message" when no event field was given
	Data string
	ID   string
}

// Parser holds the state that outlives a single connection: the last event
// ID. The zero value is ready to use.
type Parser struct {
	// LastEventID is the "last event ID string" of the event source.
	LastEventID string
	// OnRetry, if set, is called with each valid retry field value.
	OnRetry func(time.Duration)
}

// Parse reads one stream from r and calls emit for each dispatched event, in
// order. It returns the read error (io.EOF is returned as nil). A trailing
// incomplete event or line is discarded, as the spec requires.
func Parse(r io.Reader, emit func(Event)) { var p Parser; _ = p.Parse(r, emit) }

// Parse is like the package-level Parse but keeps LastEventID and OnRetry.
func (p *Parser) Parse(r io.Reader, emit func(Event)) error {
	br := bufio.NewReader(r)
	var (
		data      strings.Builder
		eventType string
		idBuf     string // per-stream buffer; starts empty (spec), unlike LastEventID
		line      []byte
		skipLF    bool // previous line ended with CR; drop an immediately following LF
		first     = true
	)
	dispatch := func() {
		p.LastEventID = idBuf
		if data.Len() == 0 {
			eventType = ""
			return
		}
		d := strings.TrimSuffix(data.String(), "\n")
		t := eventType
		if t == "" {
			t = "message"
		}
		data.Reset()
		eventType = ""
		emit(Event{Type: t, Data: d, ID: p.LastEventID})
	}
	processLine := func(l string) {
		if l == "" {
			dispatch()
			return
		}
		if l[0] == ':' {
			return
		}
		name, value, found := strings.Cut(l, ":")
		if found {
			value = strings.TrimPrefix(value, " ")
		}
		switch name {
		case "event":
			eventType = value
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		case "id":
			if !strings.ContainsRune(value, 0) {
				idBuf = value
			}
		case "retry":
			if value == "" {
				return
			}
			for _, c := range value {
				if c < '0' || c > '9' {
					return
				}
			}
			// Values above MaxRetry (including ones that overflow int64) are
			// clamped to MaxRetry rather than ignored.
			ms, err := strconv.ParseInt(value, 10, 64)
			if err != nil || ms > int64(MaxRetry/time.Millisecond) {
				ms = int64(MaxRetry / time.Millisecond)
			}
			if p.OnRetry != nil {
				p.OnRetry(time.Duration(ms) * time.Millisecond)
			}
		}
	}
	for {
		c, err := br.ReadByte()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if first {
			first = false
			if c == 0xEF {
				if b, _ := br.Peek(2); len(b) == 2 && b[0] == 0xBB && b[1] == 0xBF {
					_, _ = br.Discard(2)
					continue
				}
			}
		}
		if skipLF {
			skipLF = false
			if c == '\n' {
				continue
			}
		}
		switch c {
		case '\r':
			skipLF = true
			processLine(decodeUTF8(line))
			line = line[:0]
		case '\n':
			processLine(decodeUTF8(line))
			line = line[:0]
		default:
			line = append(line, c)
		}
	}
}

// decodeUTF8 decodes b as UTF-8 the way the WHATWG Encoding standard does:
// each maximal malformed subpart becomes one U+FFFD.
func decodeUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || n > 1 {
			sb.WriteString(string(b[i : i+n]))
			i += n
			continue
		}
		// Invalid: consume the lead byte and any continuation bytes that
		// are still valid for it.
		lo, hi := byte(0x80), byte(0xBF)
		need := 0
		switch c := b[i]; {
		case c >= 0xC2 && c <= 0xDF:
			need = 1
		case c >= 0xE0 && c <= 0xEF:
			need = 2
			if c == 0xE0 {
				lo = 0xA0
			} else if c == 0xED {
				hi = 0x9F
			}
		case c >= 0xF0 && c <= 0xF4:
			need = 3
			if c == 0xF0 {
				lo = 0x90
			} else if c == 0xF4 {
				hi = 0x8F
			}
		}
		i++
		for ; need > 0 && i < len(b) && b[i] >= lo && b[i] <= hi; need-- {
			i++
			lo, hi = 0x80, 0xBF
		}
		sb.WriteRune(utf8.RuneError)
	}
	return sb.String()
}
