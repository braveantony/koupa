package board

import (
	"encoding/json"
	"strings"
	"testing"
)

// The board shows the Pod from each hello, so hello must name the Pod of
// this connection with the same fields as /api/whoami.
func TestHelloCarriesPod(t *testing.T) {
	b := string(encodeHello(7, "v2", "koupa-abc", "uid-123"))
	want := "event: hello\nid: 7\ndata: "
	if !strings.HasPrefix(b, want) || !strings.HasSuffix(b, "\n\n") {
		t.Fatalf("block %q", b)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(b, want), "\n\n")), &d); err != nil {
		t.Fatal(err)
	}
	if d["version"] != "v2" || d["pod_name"] != "koupa-abc" || d["pod_uid"] != "uid-123" || len(d) != 3 {
		t.Fatalf("hello data %v", d)
	}
}
