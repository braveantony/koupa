package timeline

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 9, 6, 50, 0, 0, time.UTC)

func at(sec float64) *time.Time {
	t := t0.Add(time.Duration(sec * float64(time.Second)))
	return &t
}

func boolp(b bool) *bool { return &b }

func TestDeletionStartUsesFirstObservation(t *testing.T) {
	g30, g0 := int64(30), int64(0)
	obs := []PodObservation{
		{CollectedAt: *at(1)}, // not deleting yet
		{CollectedAt: *at(2.3), DeletionTimestamp: at(32), GracePeriodSeconds: &g30}, // first deletion
		{CollectedAt: *at(5), DeletionTimestamp: at(5), GracePeriodSeconds: &g0},     // grace shortened later
	}
	est, ok := DeletionStart(obs)
	if !ok {
		t.Fatal("no estimate")
	}
	if !est.Start.Equal(*at(2)) {
		t.Errorf("start = %v, want %v", est.Start, *at(2))
	}
	if !est.ObservedAt.Equal(*at(2.3)) {
		t.Errorf("observed = %v", est.ObservedAt)
	}
	// deletionTimestamp has second precision, and the request cannot be
	// later than the first observation.
	if !est.Upper.Equal(*at(2.3)) {
		t.Errorf("upper = %v, want first observation", est.Upper)
	}
	if _, ok := DeletionStart(obs[:1]); ok {
		t.Error("estimate without any deletionTimestamp")
	}
}

func baseMeta() PodLogMeta {
	return PodLogMeta{
		PodUID: "u1", CollectStart: *at(0), StartupAt: at(10),
		Graceful: boolp(false), SigtermAt: at(40),
	}
}

func TestCoverageNeedsStartMarker(t *testing.T) {
	m := baseMeta()
	if r := Coverage(m); !r.Complete {
		t.Fatalf("baseline not complete: %v", r.Reasons)
	}
	m.StartupAt = nil
	if r := Coverage(m); r.Complete {
		t.Error("complete without a startup marker")
	}
	// Startup before collection started, and not read from the beginning.
	m = baseMeta()
	m.StartupAt = at(-5)
	if r := Coverage(m); r.Complete {
		t.Error("complete although the startup marker predates collection")
	}
	m.StartupFromBeginning = true
	if r := Coverage(m); !r.Complete {
		t.Errorf("read from the beginning saw startup, still incomplete: %v", r.Reasons)
	}
}

func TestCoverageStep0EndsAtSigterm(t *testing.T) {
	m := baseMeta() // graceful=false, sigterm present, no exit line
	r := Coverage(m)
	if !r.Complete || r.EndMarker != "sigterm" {
		t.Errorf("step 0: %+v", r)
	}
	m.Graceful = boolp(true) // graceful needs the exit line
	if r := Coverage(m); r.Complete {
		t.Error("graceful Pod complete without the exit line")
	}
	m.ExitAt = at(55)
	if r := Coverage(m); !r.Complete || r.EndMarker != "exit" {
		t.Errorf("graceful with exit: %+v", r)
	}
	// Still running when the measurement stopped: covered up to the stop.
	m = baseMeta()
	m.SigtermAt = nil
	m.RunningAtStop = true
	if r := Coverage(m); !r.Complete || r.EndMarker != "running" {
		t.Errorf("running at stop: %+v", r)
	}
	m.Graceful = nil // mode unknown
	m.RunningAtStop = false
	if r := Coverage(m); r.Complete {
		t.Error("complete with unknown graceful mode")
	}
}

func TestCoverageGapNotFilledIsUncovered(t *testing.T) {
	m := baseMeta()
	m.Gaps = []Gap{{After: *at(20), Reason: "stream error", Refilled: true, Verified: false}}
	r := Coverage(m)
	if r.Complete {
		t.Fatal("unverified gap counted as covered")
	}
	if !strings.Contains(strings.Join(r.Reasons, ";"), "缺口") {
		t.Errorf("reasons do not name the gap: %v", r.Reasons)
	}
	m.Gaps[0].Refilled = false
	if r := Coverage(m); r.Complete {
		t.Error("gap never refilled counted as covered")
	}
	m.Gaps[0].Refilled, m.Gaps[0].Verified = true, true
	if r := Coverage(m); !r.Complete {
		t.Errorf("verified refill: %v", r.Reasons)
	}
	m.BadLines = 1
	if r := Coverage(m); r.Complete {
		t.Error("damaged line counted as covered")
	}
}

func TestFinishedAtDoesNotProveCoverage(t *testing.T) {
	m := baseMeta()
	m.SigtermAt = nil
	m.FinishedAt = at(41)
	r := Coverage(m)
	if r.Complete {
		t.Fatal("finishedAt alone made the log complete")
	}
	if r.EndMarker != "" {
		t.Errorf("end marker = %q", r.EndMarker)
	}
}

func TestDeletionStartMissingGraceIsUnknown(t *testing.T) {
	obs := []PodObservation{{CollectedAt: *at(2.3), DeletionTimestamp: at(32)}}
	if est, ok := DeletionStart(obs); ok {
		t.Errorf("fabricated termination start %v without deletionGracePeriodSeconds", est.Start)
	}
}
