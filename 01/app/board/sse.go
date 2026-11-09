package board

import (
	"encoding/json"
	"strconv"
	"time"

	"koupa/app/broadcast"
)

// Event encoding (plan A4). Each function returns one complete block so
// it goes out in a single Write: heartbeats never land inside an event.

type helloData struct {
	Version string `json:"version"`
}

type readyData struct {
	Number  string `json:"number"`
	OrderID int64  `json:"order_id"`
}

// encodeHello: id is the connection's start point.
func encodeHello(start int64, version string) []byte {
	d, _ := json.Marshal(helloData{Version: version})
	return block("hello", strconv.FormatInt(start, 10), d)
}

// encodeReady: id is the ready_seq.
func encodeReady(ev broadcast.ReadyEvent) []byte {
	d, _ := json.Marshal(readyData{Number: ev.Number, OrderID: ev.OrderID})
	return block("ready", strconv.FormatInt(ev.ReadySeq, 10), d)
}

// encodeShutdown sets the reconnect delay, then sends a shutdown event with
// an empty data line: WHATWG SSE does not dispatch an event with no data.
func encodeShutdown(retry time.Duration) []byte {
	b := []byte("retry: " + strconv.FormatInt(retry.Milliseconds(), 10) + "\n")
	return append(b, "event: shutdown\ndata:\n\n"...)
}

// heartbeat is a comment line; clients ignore it.
var heartbeat = []byte(": hb\n")

func block(event, id string, data []byte) []byte {
	b := make([]byte, 0, 32+len(id)+len(data))
	b = append(b, "event: "...)
	b = append(b, event...)
	b = append(b, "\nid: "...)
	b = append(b, id...)
	b = append(b, "\ndata: "...)
	b = append(b, data...)
	return append(b, "\n\n"...)
}
