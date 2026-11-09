package verdict

import (
	"reflect"
	"testing"
	"time"
)

var (
	t0        = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	txTimeout = 60 * time.Second
)

func ip(n int) *int                     { return &n }
func i64(n int64) *int64                { return &n }
func tp(t time.Time) *time.Time         { return &t }
func ing(n int) *IngressRecord          { return &IngressRecord{Finished: true, UpstreamAttempts: ip(n)} }
func bp(b bool) *bool                   { return &b }
func dp(d time.Duration) *time.Duration { return &d }
func at(d time.Duration) time.Time      { return t0.Add(d) }

func TestCountString(t *testing.T) {
	cases := []struct {
		c    Count
		want string
	}{
		{Count{0, 12}, "已確認 0；另有 12 項未知"},
		{Count{0, 0}, "0"},
		{Count{3, 0}, "3"},
		{Count{2, 5}, "已確認 2；另有 5 項未知"},
	}
	for _, tc := range cases {
		if got := tc.c.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.c, got, tc.want)
		}
	}
	if got := (Count{0, 12}).StringUnit("個叫號"); got != "已確認 0；另有 12 個叫號未知" {
		t.Errorf("StringUnit = %q", got)
	}
	if got := (Count{4, 0}).StringUnit("個叫號"); got != "4" {
		t.Errorf("StringUnit no unknown = %q", got)
	}
}

func TestSettle(t *testing.T) {
	now := at(10 * time.Minute)
	cases := []struct {
		name string
		e    AttemptEvidence
		want bool
	}{
		{"ingress record missing", AttemptEvidence{FullSuccess: true}, false},
		{"ingress not finished", AttemptEvidence{Ingress: &IngressRecord{Finished: false, UpstreamAttempts: ip(1)}, FullSuccess: true}, false},
		{"upstream attempts field missing (nil)", AttemptEvidence{Ingress: &IngressRecord{Finished: true}, FullSuccess: true}, false},
		{"0 upstream attempts", AttemptEvidence{Ingress: ing(0)}, true},
		{"1 + full success", AttemptEvidence{Ingress: ing(1), FullSuccess: true}, true},
		{"1 + committed", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: &AppEnd{TxOutcome: TxCommitted}}, true},
		{"1 + rolled_back", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: &AppEnd{TxOutcome: TxRolledBack}}, true},
		{"1 + not_started", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: &AppEnd{TxOutcome: TxNotStarted}}, true},
		{"1 + unrecognised outcome", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: &AppEnd{TxOutcome: "weird"}}, false},
		{"1 + commit_unknown 59s after COMMIT", AttemptEvidence{Ingress: ing(1), AppReceived: true,
			AppEnd: &AppEnd{TxOutcome: TxCommitUnknown, CommitSentAt: tp(now.Add(-59 * time.Second))}}, false},
		{"1 + commit_unknown exactly 60s after COMMIT", AttemptEvidence{Ingress: ing(1), AppReceived: true,
			AppEnd: &AppEnd{TxOutcome: TxCommitUnknown, CommitSentAt: tp(now.Add(-60 * time.Second))}}, false},
		{"1 + commit_unknown 61s after COMMIT", AttemptEvidence{Ingress: ing(1), AppReceived: true,
			AppEnd: &AppEnd{TxOutcome: TxCommitUnknown, CommitSentAt: tp(now.Add(-61 * time.Second))}}, true},
		{"1 + commit_unknown with COMMIT time missing", AttemptEvidence{Ingress: ing(1), AppReceived: true,
			AppEnd: &AppEnd{TxOutcome: TxCommitUnknown}}, false},
		{"1 + received only + Pod exited 59s ago", AttemptEvidence{Ingress: ing(1), AppReceived: true, LogCovered: true,
			PodExitedAt: tp(now.Add(-59 * time.Second))}, false},
		{"1 + received only + Pod exited 61s ago", AttemptEvidence{Ingress: ing(1), AppReceived: true, LogCovered: true,
			PodExitedAt: tp(now.Add(-61 * time.Second))}, true},
		{"1 + received only + Pod exited 61s ago but log not covered", AttemptEvidence{Ingress: ing(1), AppReceived: true,
			PodExitedAt: tp(now.Add(-61 * time.Second))}, false},
		{"1 + received only + Pod still running", AttemptEvidence{Ingress: ing(1), AppReceived: true, LogCovered: true}, false},
		{"1 + no received", AttemptEvidence{Ingress: ing(1), LogCovered: true, PodExitedAt: tp(now.Add(-time.Hour))}, false},
		{"2 upstream attempts even with full success and committed", AttemptEvidence{Ingress: ing(2), FullSuccess: true,
			AppReceived: true, AppEnd: &AppEnd{TxOutcome: TxCommitted}, LogCovered: true}, false},
		{"3 upstream attempts", AttemptEvidence{Ingress: ing(3)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Settle(tc.e, now, txTimeout)
			if got.Settled != tc.want {
				t.Errorf("Settled = %v (reason %q), want %v", got.Settled, got.Reason, tc.want)
			}
			if got.Reason == "" {
				t.Errorf("empty Reason")
			}
		})
	}
	// A zero or negative transaction_timeout is not evidence that time has passed.
	for _, e := range []AttemptEvidence{
		{Ingress: ing(1), AppReceived: true, AppEnd: &AppEnd{TxOutcome: TxCommitUnknown, CommitSentAt: tp(now.Add(-time.Hour))}},
		{Ingress: ing(1), AppReceived: true, LogCovered: true, PodExitedAt: tp(now.Add(-time.Hour))},
	} {
		if got := Settle(e, now, 0); got.Settled {
			t.Errorf("txTimeout 0: settled (%q), want not settled", got.Reason)
		}
	}
}

func TestState(t *testing.T) {
	end3 := func(writeErr, flushErr, canceled *bool) *AppEnd {
		return &AppEnd{TxOutcome: TxCommitted, WriteErr: writeErr, FlushErr: flushErr, CtxCanceled: canceled}
	}
	end := func(writeErr, canceled bool) *AppEnd { return end3(bp(writeErr), bp(false), bp(canceled)) }
	cases := []struct {
		name    string
		e       AttemptEvidence
		fullErr bool
		want    StateKind
	}{
		{"full 503 -> ClientGotError even if App wrote OK", AttemptEvidence{Ingress: ing(1), AppReceived: true,
			AppEnd: end(false, false), LogCovered: true}, true, ClientGotError},
		{"full 4xx with ingress retried -> ClientGotError", AttemptEvidence{Ingress: ing(2)}, true, ClientGotError},
		{"ingress missing", AttemptEvidence{AppReceived: true, AppEnd: end(false, false), LogCovered: true}, false, Unknown},
		{"ingress not finished", AttemptEvidence{Ingress: &IngressRecord{UpstreamAttempts: ip(1)}, AppReceived: true, LogCovered: true}, false, Unknown},
		{"ingress attempts nil", AttemptEvidence{Ingress: &IngressRecord{Finished: true}, LogCovered: true}, false, Unknown},
		{"ingress 2 attempts", AttemptEvidence{Ingress: ing(2), AppReceived: true, AppEnd: end(false, false), LogCovered: true}, false, Unknown},
		{"ingress 0 attempts", AttemptEvidence{Ingress: ing(0)}, false, NotReachedApp},
		{"1 + no App record", AttemptEvidence{Ingress: ing(1), LogCovered: true}, false, Unknown},
		{"1 + received not ended, log covered", AttemptEvidence{Ingress: ing(1), AppReceived: true, LogCovered: true}, false, ReachedNotEnded},
		{"1 + received not ended, log NOT covered", AttemptEvidence{Ingress: ing(1), AppReceived: true}, false, Unknown},
		{"1 + end with write error", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: end(true, false), LogCovered: true}, false, WriteFailedOrCanceled},
		{"1 + end with ctx canceled", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: end(false, true), LogCovered: true}, false, WriteFailedOrCanceled},
		{"1 + end write and flush ok", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: end(false, false), LogCovered: true}, false, WriteFlushOK},
		{"1 + end with flush error", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: end3(bp(false), bp(true), bp(false)), LogCovered: true}, false, WriteFailedOrCanceled},
		{"1 + end: write error, other fields missing", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: end3(bp(true), nil, nil), LogCovered: true}, false, WriteFailedOrCanceled},
		{"1 + end: write field missing", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: end3(nil, bp(false), bp(false)), LogCovered: true}, false, Unknown},
		{"1 + end: flush field missing", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: end3(bp(false), nil, bp(false)), LogCovered: true}, false, Unknown},
		{"1 + end: cancel field missing", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: end3(bp(false), bp(false), nil), LogCovered: true}, false, Unknown},
		{"1 + end: all write/flush/cancel fields missing", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: end3(nil, nil, nil), LogCovered: true}, false, Unknown},
		{"1 + end but log not covered", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: end(false, false)}, false, Unknown},
		{"1 + end without received (contradiction)", AttemptEvidence{Ingress: ing(1), AppEnd: end(false, false), LogCovered: true}, false, Unknown},
		{"1 + records flagged contradictory", AttemptEvidence{Ingress: ing(1), AppReceived: true, AppEnd: end(false, false),
			LogCovered: true, Contradictory: true}, false, Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := State(tc.e, tc.fullErr); got != tc.want {
				t.Errorf("State = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCause(t *testing.T) {
	prec := 100 * time.Millisecond
	routed := func() AttemptEvidence {
		return AttemptEvidence{
			Ingress:      &IngressRecord{Finished: true, UpstreamAttempts: ip(1), UpstreamPod: "pod-a", IngressStart: tp(at(5 * time.Second))},
			PodTermStart: tp(at(4 * time.Second)),
			Precision:    dp(prec),
		}
	}
	midway := func() AttemptEvidence {
		return AttemptEvidence{
			Ingress:       ing(1),
			AppReceived:   true,
			AppReceivedAt: tp(at(1 * time.Second)),
			PodExitedAt:   tp(at(2 * time.Second)),
			LogCovered:    true,
		}
	}
	mod := func(base func() AttemptEvidence, f func(*AttemptEvidence)) AttemptEvidence {
		e := base()
		f(&e)
		return e
	}
	cases := []struct {
		name  string
		e     AttemptEvidence
		state StateKind
		want  CauseKind
	}{
		{"routed after termination: complete evidence", routed(), Unknown, CauseRoutedAfterTermination},
		{"routed: ingress missing", mod(routed, func(e *AttemptEvidence) { e.Ingress = nil }), Unknown, CauseUnknown},
		{"routed: ingress not finished", mod(routed, func(e *AttemptEvidence) { e.Ingress.Finished = false }), Unknown, CauseUnknown},
		{"routed: attempts nil", mod(routed, func(e *AttemptEvidence) { e.Ingress.UpstreamAttempts = nil }), Unknown, CauseUnknown},
		{"routed: attempts 2", mod(routed, func(e *AttemptEvidence) { e.Ingress.UpstreamAttempts = ip(2) }), Unknown, CauseUnknown},
		{"routed: attempts 0", mod(routed, func(e *AttemptEvidence) { e.Ingress.UpstreamAttempts = ip(0) }), NotReachedApp, CauseUnknown},
		{"routed: upstream not a Pod", mod(routed, func(e *AttemptEvidence) { e.Ingress.UpstreamPod = "" }), Unknown, CauseUnknown},
		{"routed: ingress start missing", mod(routed, func(e *AttemptEvidence) { e.Ingress.IngressStart = nil }), Unknown, CauseUnknown},
		{"routed: Pod term start missing", mod(routed, func(e *AttemptEvidence) { e.PodTermStart = nil }), Unknown, CauseUnknown},
		{"routed: precision missing", mod(routed, func(e *AttemptEvidence) { e.Precision = nil }), Unknown, CauseUnknown},
		{"routed: later but within precision", mod(routed, func(e *AttemptEvidence) {
			e.Ingress.IngressStart = tp(at(4*time.Second + prec))
		}), Unknown, CauseUnknown},
		{"routed: before termination", mod(routed, func(e *AttemptEvidence) {
			e.Ingress.IngressStart = tp(at(3 * time.Second))
		}), Unknown, CauseUnknown},
		{"exited mid-request: complete evidence", midway(), ReachedNotEnded, CauseExitedMidRequest},
		{"exited mid-request: state not ReachedNotEnded", midway(), Unknown, CauseUnknown},
		{"exited mid-request: received time missing", mod(midway, func(e *AttemptEvidence) { e.AppReceivedAt = nil }), ReachedNotEnded, CauseUnknown},
		{"exited mid-request: exit time missing", mod(midway, func(e *AttemptEvidence) { e.PodExitedAt = nil }), ReachedNotEnded, CauseUnknown},
		{"exited mid-request: received after exit", mod(midway, func(e *AttemptEvidence) {
			e.AppReceivedAt = tp(at(3 * time.Second))
		}), ReachedNotEnded, CauseUnknown},
		{"exited mid-request: received equals exit", mod(midway, func(e *AttemptEvidence) {
			e.AppReceivedAt = tp(at(2 * time.Second))
		}), ReachedNotEnded, CauseUnknown},
		{"exited mid-request: client got full success", mod(midway, func(e *AttemptEvidence) { e.FullSuccess = true }), ReachedNotEnded, CauseUnknown},
		{"no evidence at all", AttemptEvidence{}, Unknown, CauseUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Cause(tc.e, tc.state); got != tc.want {
				t.Errorf("Cause = %v, want %v", got, tc.want)
			}
		})
	}
}

func rows(seqs ...int64) []ReadyRow {
	out := make([]ReadyRow, len(seqs))
	for i, s := range seqs {
		out[i] = ReadyRow{Seq: s, OrderID: 1000 + s}
	}
	return out
}

func TestMissing(t *testing.T) {
	cases := []struct {
		name string
		in   MissingInput
		want MissingResult
	}{
		{"single miss", MissingInput{
			DBComplete: true, DBReady: rows(1, 2, 3, 4, 5), Cursor: 5,
			Boards: []BoardSeq{{Name: "b1", Start: i64(0), Received: []int64{1, 2, 4, 5}, CaughtUp: true, Complete: true}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {1, 0}}, NotIncreasing: map[string]bool{"b1": false}}},
		{"multiple misses across boards", MissingInput{
			DBComplete: true, DBReady: rows(1, 2, 3, 4, 5, 6), Cursor: 6,
			Boards: []BoardSeq{
				{Name: "b1", Start: i64(0), Received: []int64{1, 4, 6}, CaughtUp: true, Complete: true},
				{Name: "b2", Start: i64(0), Received: []int64{1, 2, 3, 4, 5, 6}, CaughtUp: true, Complete: true},
			},
		}, MissingResult{PerBoard: map[string]Count{"b1": {3, 0}, "b2": {0, 0}}, NotIncreasing: map[string]bool{"b1": false, "b2": false}}},
		{"duplicate received", MissingInput{
			DBComplete: true, DBReady: rows(1, 2, 3), Cursor: 3,
			Boards: []BoardSeq{{Name: "b1", Start: i64(0), Received: []int64{1, 2, 2, 3, 3, 3}, CaughtUp: true, Complete: true}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {0, 0}}, Duplicates: map[string][]int64{"b1": {2, 3}},
			NotIncreasing: map[string]bool{"b1": true}}},
		{"recovered after reconnect (replay fills the gap)", MissingInput{
			DBComplete: true, DBReady: rows(1, 2, 3, 4, 5), Cursor: 5,
			// connection 1 got 1,2; reconnect with Last-Event-ID 2 replays 3,4; then live 5.
			Boards: []BoardSeq{{Name: "b1", Start: i64(0), Received: []int64{1, 2, 3, 4, 5}, CaughtUp: true, Complete: true}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {0, 0}}, NotIncreasing: map[string]bool{"b1": false}}},
		{"recovered late out of order: not missing, but not increasing", MissingInput{
			DBComplete: true, DBReady: rows(1, 2, 3, 4), Cursor: 4,
			Boards: []BoardSeq{{Name: "b1", Start: i64(0), Received: []int64{1, 2, 4, 3}, CaughtUp: true, Complete: true}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {0, 0}}, NotIncreasing: map[string]bool{"b1": true}}},
		{"start boundary: seq equal to start not counted", MissingInput{
			DBComplete: true, DBReady: rows(1, 2, 3, 4, 5), Cursor: 5,
			Boards: []BoardSeq{{Name: "b1", Start: i64(3), Received: []int64{4, 5}, CaughtUp: true, Complete: true}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {0, 0}}, NotIncreasing: map[string]bool{"b1": false}}},
		{"start boundary: seq start+1 counted", MissingInput{
			DBComplete: true, DBReady: rows(1, 2, 3, 4, 5), Cursor: 5,
			Boards: []BoardSeq{{Name: "b1", Start: i64(3), Received: []int64{5}, CaughtUp: true, Complete: true}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {1, 0}}, NotIncreasing: map[string]bool{"b1": false}}},
		{"cursor boundary: seq equal to C counted, above C not", MissingInput{
			DBComplete: true, DBReady: rows(1, 2, 3, 4, 5, 6), Cursor: 5,
			Boards: []BoardSeq{{Name: "b1", Start: i64(0), Received: []int64{1, 2, 3, 4}, CaughtUp: true, Complete: true}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {1, 0}}, NotIncreasing: map[string]bool{"b1": false}}},
		{"empty set", MissingInput{
			DBComplete: true, Cursor: 0,
			Boards: []BoardSeq{{Name: "b1", Start: i64(0), CaughtUp: true, Complete: true}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {0, 0}}, NotIncreasing: map[string]bool{"b1": false}}},
		{"empty received with DB rows: all missing", MissingInput{
			DBComplete: true, DBReady: rows(1, 2), Cursor: 2,
			Boards: []BoardSeq{{Name: "b1", Start: i64(0), CaughtUp: true, Complete: true}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {2, 0}}, NotIncreasing: map[string]bool{"b1": false}}},
		{"unsettled seqs are unknown, not missing", MissingInput{
			DBComplete: true, DBReady: rows(1, 2, 3, 4, 7), Cursor: 4, UnsettledSeqs: map[int64]bool{3: true, 7: true, 2: true},
			Boards: []BoardSeq{{Name: "b1", Start: i64(0), Received: []int64{1, 2}, CaughtUp: true, Complete: true}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {1, 1}}, NotIncreasing: map[string]bool{"b1": false}}},
		{"not caught up: all in-range not received are unknown", MissingInput{
			DBComplete: true, DBReady: rows(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15), Cursor: 15,
			Boards: []BoardSeq{{Name: "b1", Start: i64(0), Received: []int64{1, 2, 3}, CaughtUp: false, Complete: true}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {0, 12}}, NotIncreasing: map[string]bool{"b1": false}}},
		{"board record incomplete: in-range not received are unknown", MissingInput{
			DBComplete: true, DBReady: rows(1, 2, 3, 4), Cursor: 4,
			Boards: []BoardSeq{{Name: "b1", Start: i64(0), Received: []int64{1}, CaughtUp: true, Complete: false}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {0, 3}}, NotIncreasing: map[string]bool{"b1": false}}},
		{"DB/audit evidence incomplete -> invalid", MissingInput{
			DBComplete: false, DBReady: rows(1, 2), Cursor: 2,
			Boards: []BoardSeq{{Name: "b1", Start: i64(0), Received: []int64{1}, CaughtUp: true, Complete: true}},
		}, MissingResult{PerBoard: map[string]Count{"b1": {1, 0}}, NotIncreasing: map[string]bool{"b1": false}, Invalid: true}},
		{"start missing -> invalid", MissingInput{
			DBComplete: true, DBReady: rows(1, 2), Cursor: 2,
			Boards: []BoardSeq{
				{Name: "b1", Start: nil, Received: []int64{1, 2}, CaughtUp: true, Complete: true},
				{Name: "b2", Start: i64(0), Received: []int64{1}, CaughtUp: true, Complete: true},
			},
		}, MissingResult{PerBoard: map[string]Count{"b2": {1, 0}}, NotIncreasing: map[string]bool{"b1": false, "b2": false}, Invalid: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Missing(tc.in)
			if tc.want.Duplicates == nil {
				tc.want.Duplicates = map[string][]int64{}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Missing =\n %+v\nwant\n %+v", got, tc.want)
			}
		})
	}
}

func TestDuplicates(t *testing.T) {
	cases := []struct {
		name      string
		orders    map[string][]int64
		unsettled map[string]bool
		wantRefs  Count
		wantExtra Count
	}{
		{"none", map[string][]int64{"a": {1}, "b": {2}}, nil, Count{0, 0}, Count{0, 0}},
		{"empty", nil, nil, Count{0, 0}, Count{0, 0}},
		{"one ref with 3 orders", map[string][]int64{"a": {1, 2, 3}, "b": {4}}, nil, Count{1, 0}, Count{2, 0}},
		{"unsettled single order may grow", map[string][]int64{"a": {1}, "b": {2, 3}}, map[string]bool{"a": true}, Count{1, 1}, Count{1, 1}},
		{"unsettled dup may grow further", map[string][]int64{"a": {1, 2}}, map[string]bool{"a": true}, Count{1, 0}, Count{1, 1}},
		{"unsettled with no order yet", map[string][]int64{"b": {2}}, map[string]bool{"a": true}, Count{0, 1}, Count{0, 1}},
		{"unsettled ref listed with empty orders", map[string][]int64{"a": {}}, map[string]bool{"a": true}, Count{0, 1}, Count{0, 1}},
		{"unsettled flag false is ignored", map[string][]int64{"a": {1}}, map[string]bool{"a": false, "z": false}, Count{0, 0}, Count{0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refs, extra := Duplicates(tc.orders, tc.unsettled)
			if refs != tc.wantRefs || extra != tc.wantExtra {
				t.Errorf("Duplicates = %+v, %+v; want %+v, %+v", refs, extra, tc.wantRefs, tc.wantExtra)
			}
		})
	}
}
