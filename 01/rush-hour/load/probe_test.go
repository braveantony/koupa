package load

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"koupa/internal/wire"
	"koupa/rush-hour/record"
)

func TestProberUsesNewConnection(t *testing.T) {
	var conns, reqs atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		if r.URL.Path != "/api/whoami" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set(wire.HeaderPodUID, "pod-p")
		io.WriteString(w, `{"pod_uid":"pod-p"}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	var buf bytes.Buffer
	p := &Prober{Base: srv.URL, RunID: "runP", Rate: 200, W: record.NewWriter(&buf)}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, &buf)
	if len(recs) < 20 {
		t.Fatalf("only %d probes in 250ms at 200/s", len(recs))
	}
	if conns.Load() != reqs.Load() || int(reqs.Load()) != len(recs) {
		t.Fatalf("connections %d, requests %d, records %d: want all equal", conns.Load(), reqs.Load(), len(recs))
	}
	seqs := map[string]bool{}
	for _, r := range recs {
		if r.Kind != record.KindProbe || r.PodUID != "pod-p" || r.Status == nil || *r.Status != 200 ||
			r.SentAt.IsZero() || r.RespondedAt.Before(r.SentAt) || !r.FullResponse {
			t.Fatalf("bad probe record %+v", r)
		}
		seqs[r.Attempt] = true
	}
	for i := 1; i <= len(recs); i++ {
		if !seqs[wire.NewAttemptID("runP-p", uint64(i), 1)] {
			t.Fatalf("probe seq %d missing (ids not sequential from 1)", i)
		}
	}
}

func TestProberFixedRateNotWaitingForResponses(t *testing.T) {
	// A server that answers slowly must not slow the send rate.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	p := &Prober{Base: srv.URL, RunID: "runP", Rate: 100, W: record.NewWriter(&buf), Timeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(readRecords(t, &buf)); n < 10 {
		t.Fatalf("sent %d probes in 200ms at 100/s with slow responses", n)
	}
}

func TestProberRecordsNoResponse(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // refuse connections
	var buf bytes.Buffer
	p := &Prober{Base: base, RunID: "runP", Rate: 50, W: record.NewWriter(&buf)}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, &buf)
	if len(recs) == 0 {
		t.Fatal("no records")
	}
	for _, r := range recs {
		if r.Status != nil || r.PodUID != "" || r.ErrKind != record.ErrRefused || r.FullResponse {
			t.Fatalf("want refused, no status/pod_uid: %+v", r)
		}
	}
	if strings.Contains(buf.String(), `"pod_uid":null`) {
		t.Fatal("unexpected null")
	}
}

func TestProberForcesNewConnectionWithSuppliedKeepAliveClient(t *testing.T) {
	var conns, reqs atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		io.WriteString(w, `{}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	keepAlive := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 10}} // would reuse connections
	var buf bytes.Buffer
	p := &Prober{Client: keepAlive, Base: srv.URL, RunID: "runP", Rate: 100, W: record.NewWriter(&buf)}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if reqs.Load() < 10 || conns.Load() != reqs.Load() {
		t.Fatalf("connections %d, requests %d: want equal", conns.Load(), reqs.Load())
	}
}

func TestProberPropagatesWriterError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	p := &Prober{Base: srv.URL, RunID: "runP", Rate: 100, W: record.NewWriter(failWriter{})}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err == nil {
		t.Fatal("expected record write error from Run")
	}
}
