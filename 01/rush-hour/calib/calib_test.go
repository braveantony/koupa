package calib

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

const target = "pod-A"

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const spacing = 30 * time.Millisecond

// mk builds a probe sent at `at` and answered 5ms later.
func mk(at time.Time, hit bool) Probe {
	uid := "pod-B"
	if hit {
		uid = target
	}
	return Probe{SentAt: at, RespondedAt: at.Add(5 * time.Millisecond), PodUID: uid}
}

// bernoulli returns a seeded sequence with exactly `hits` true values out of n.
func bernoulli(seed int64, n, hits int) []bool {
	s := make([]bool, n)
	for i := 0; i < hits; i++ {
		s[i] = true
	}
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(n, func(i, j int) { s[i], s[j] = s[j], s[i] })
	return s
}

func seq(start time.Time, hits []bool) []Probe {
	out := make([]Probe, len(hits))
	for i, h := range hits {
		out[i] = mk(start.Add(time.Duration(i)*spacing), h)
	}
	return out
}

type scenario struct {
	in       Input
	lastHit  Probe
	deletion time.Time
}

// build: 300 pre-deletion probes with the given pattern (p = hits/300), then
// post probes (given pattern) until the last hit, then `after` non-hit probes.
func build(pre, post []bool, after int) scenario {
	del := t0.Add(time.Duration(len(pre)) * spacing)
	probes := seq(t0, pre)
	postP := seq(del.Add(spacing), post)
	probes = append(probes, postP...)
	last := postP[len(postP)-1]
	for i := len(post) - 1; i >= 0; i-- {
		if post[i] {
			last = postP[i]
			break
		}
	}
	tail := last.SentAt.Add(spacing)
	probes = append(probes, seq(tail, make([]bool, after))...)
	return scenario{
		in: Input{Probes: probes, TargetPodUID: target, DeletionStart: del, FreshConnections: true,
			PreStopEnd: del.Add(20 * time.Second)},
		lastHit:  last,
		deletion: del,
	}
}

func goodPre() []bool { return bernoulli(1, 300, 100) } // p = 1/3

func goodPost() []bool {
	p := bernoulli(2, 90, 30)
	p[len(p)-1] = true
	return p
}

func cfg() Config { return DefaultConfig() }

func TestLowerUsesSentAtNotAppTime(t *testing.T) {
	s := build(goodPre(), goodPost(), 30)
	// Pretend the App received the last hit much later than it was sent.
	for i := range s.in.Probes {
		if s.in.Probes[i].SentAt.Equal(s.lastHit.SentAt) {
			s.in.Probes[i].RespondedAt = s.lastHit.SentAt.Add(3 * time.Second)
		}
	}
	got := Estimate(s.in, cfg())
	if !got.Lower.Equal(s.lastHit.SentAt) {
		t.Fatalf("Lower = %v, want SentAt %v", got.Lower, s.lastHit.SentAt)
	}
}

func TestLowerUsesIngressWhenPerAttempt(t *testing.T) {
	s := build(goodPre(), goodPost(), 30)
	s.in.PerAttemptIngress = true
	sel := s.lastHit.SentAt.Add(2 * time.Millisecond)
	for i := range s.in.Probes {
		if s.in.Probes[i].SentAt.Equal(s.lastHit.SentAt) {
			s.in.Probes[i].IngressSelectedAt = &sel
		}
	}
	got := Estimate(s.in, cfg())
	if !got.Lower.Equal(sel) {
		t.Fatalf("Lower = %v, want ingress time %v", got.Lower, sel)
	}
}

func TestUpperModelK(t *testing.T) {
	s := build(goodPre(), goodPost(), 30)
	got := Estimate(s.in, cfg())
	if got.UpperKind != "model" {
		t.Fatalf("UpperKind = %q (reason %q, z %v), want model", got.UpperKind, got.InvalidReason, got.RunsZ)
	}
	if math.Abs(got.P-1.0/3) > 1e-9 || got.K != 18 || got.N != 300 {
		t.Fatalf("P=%v K=%d N=%d, want 1/3, 18, 300", got.P, got.K, got.N)
	}
	// 18th non-hit probe after L: first one is sent one spacing after the last hit.
	want := s.lastHit.SentAt.Add(18*spacing + 5*time.Millisecond)
	if got.Upper == nil || !got.Upper.Equal(want) {
		t.Fatalf("Upper = %v, want %v", got.Upper, want)
	}
	if !got.Valid {
		t.Fatalf("expected valid, got %q", got.InvalidReason)
	}
}

func TestUpperModelNeedsKProbes(t *testing.T) {
	s := build(goodPre(), goodPost(), 17)
	got := Estimate(s.in, cfg())
	if got.UpperKind != "none" || got.Upper != nil {
		t.Fatalf("17 non-hit probes must not give an upper bound, got %q", got.UpperKind)
	}
}

func TestDirectUpperPreferred(t *testing.T) {
	s := build(goodPre(), goodPost(), 30)
	d := s.deletion.Add(3 * time.Second)
	s.in.DirectUpper = &d
	got := Estimate(s.in, cfg())
	if got.UpperKind != "direct" || got.Upper == nil || !got.Upper.Equal(d) {
		t.Fatalf("got kind %q upper %v, want direct %v", got.UpperKind, got.Upper, d)
	}
}

func TestDirectUpperWorksWithoutModel(t *testing.T) {
	s := build(goodPre()[:50], goodPost(), 30)
	d := s.deletion.Add(3 * time.Second)
	s.in.DirectUpper = &d
	got := Estimate(s.in, cfg())
	if got.UpperKind != "direct" || !got.Valid {
		t.Fatalf("kind %q valid %v reason %q", got.UpperKind, got.Valid, got.InvalidReason)
	}
}

func TestNoUpperWhenFewSamples(t *testing.T) {
	pre := goodPre()[:99]
	s := build(pre, goodPost(), 30)
	got := Estimate(s.in, cfg())
	if got.UpperKind != "none" || got.Valid || got.InvalidReason != "no_upper_bound" {
		t.Fatalf("kind %q valid %v reason %q", got.UpperKind, got.Valid, got.InvalidReason)
	}
	// Exactly 100 is enough (boundary), assuming the sequence passes the runs test.
	s = build(goodPre()[:100], goodPost(), 30)
	if got := Estimate(s.in, cfg()); got.N != 100 {
		t.Fatalf("N = %d, want 100", got.N)
	}
}

func TestNoProbesWithoutResponseCounted(t *testing.T) {
	s := build(goodPre(), goodPost(), 30)
	// Drop responses of 250 pre-deletion probes: only 50 remain countable.
	for i := 0; i < 250; i++ {
		s.in.Probes[i].PodUID = ""
	}
	got := Estimate(s.in, cfg())
	if got.N != 50 || got.UpperKind != "none" {
		t.Fatalf("N=%d kind=%q, want 50 / none", got.N, got.UpperKind)
	}
}

func TestNoUpperWhenPZero(t *testing.T) {
	pre := make([]bool, 300)
	s := build(pre, goodPost(), 30)
	got := Estimate(s.in, cfg())
	if got.UpperKind != "none" || got.P != 0 {
		t.Fatalf("kind %q p %v", got.UpperKind, got.P)
	}
}

func TestRunsTestRejectsCorrelated(t *testing.T) {
	alt := make([]bool, 300)
	for i := range alt {
		alt[i] = i%2 == 0
	}
	_, p := RunsTest(alt)
	if !(p < 0.05) {
		t.Fatalf("alternating p = %v, want < 0.05", p)
	}
	s := build(alt, goodPost(), 30)
	got := Estimate(s.in, cfg())
	if got.UpperKind != "none" {
		t.Fatalf("model must be rejected for alternating pre-sequence, got %q", got.UpperKind)
	}
	// Positively correlated: long blocks.
	blocks := make([]bool, 300)
	for i := range blocks {
		blocks[i] = (i/10)%3 == 0
	}
	if _, p := RunsTest(blocks); !(p < 0.05) {
		t.Fatalf("blocks p = %v, want < 0.05", p)
	}
}

func TestRunsTestAcceptsRandom(t *testing.T) {
	_, p := RunsTest(goodPre())
	if p < 0.05 {
		t.Fatalf("seeded random sequence rejected, p = %v", p)
	}
}

func TestRunsTestDegenerate(t *testing.T) {
	cases := map[string][]bool{
		"nil":       nil,
		"single":    {true},
		"all true":  {true, true, true, true},
		"all false": {false, false, false},
	}
	for name, in := range cases {
		z, p := RunsTest(in)
		if !math.IsNaN(z) || !math.IsNaN(p) {
			t.Errorf("%s: z=%v p=%v, want NaN/NaN", name, z, p)
		}
	}
}

func TestRateChangeRejectsModel(t *testing.T) {
	post := bernoulli(3, 90, 80) // ~89% hit rate vs p = 1/3
	post[len(post)-1] = true
	s := build(goodPre(), post, 30)
	got := Estimate(s.in, cfg())
	if got.UpperKind != "none" {
		t.Fatalf("model must be rejected on rate change, got %q", got.UpperKind)
	}
	// Control: same shape, similar rate is accepted (see TestUpperModelK).
}

func TestInvalidWhenNoHitAfterDeletion(t *testing.T) {
	post := make([]bool, 90)
	s := build(goodPre(), post, 30)
	d := s.deletion.Add(time.Second)
	s.in.DirectUpper = &d // even with an upper bound
	got := Estimate(s.in, cfg())
	if got.Valid || got.InvalidReason != "no_hit_after_deletion" {
		t.Fatalf("valid %v reason %q", got.Valid, got.InvalidReason)
	}
}

func TestInvalidWhenUpperTooCloseToPreStopEnd(t *testing.T) {
	s := build(goodPre(), goodPost(), 30)
	d := s.in.PreStopEnd.Add(-4 * time.Second)
	s.in.DirectUpper = &d
	got := Estimate(s.in, cfg())
	if got.Valid || got.InvalidReason != "upper_too_close_to_prestop_end" {
		t.Fatalf("valid %v reason %q", got.Valid, got.InvalidReason)
	}
	d = s.in.PreStopEnd.Add(-5 * time.Second) // exactly 5s early is allowed
	got = Estimate(s.in, cfg())
	if !got.Valid {
		t.Fatalf("U = PreStopEnd-5s must be valid, got %q", got.InvalidReason)
	}
}

func TestCandidateNeedsFive(t *testing.T) {
	del := t0
	mkS := func(sec float64, valid bool) Sample {
		u := del.Add(time.Duration(sec * float64(time.Second)))
		return Sample{DeletionStart: del, Upper: &u, UpperKind: "direct", Valid: valid}
	}
	four := []Sample{mkS(3, true), mkS(4, true), mkS(5, true), mkS(6, true), mkS(30, false)}
	if _, ok := Candidate(four); ok {
		t.Fatal("4 valid samples must not give a candidate")
	}
	five := append(four, mkS(7.5, true))
	sec, ok := Candidate(five)
	if !ok || sec != 7.5 {
		t.Fatalf("got %v %v, want 7.5 true (invalid 30s sample ignored)", sec, ok)
	}
}

func TestOverlappingProbesRaiseModelUpper(t *testing.T) {
	s := build(goodPre(), goodPost(), 30)
	// A probe sent before the 18th miss is answered much later: U must follow it.
	late := s.lastHit.SentAt.Add(5 * spacing)
	for i := range s.in.Probes {
		if s.in.Probes[i].SentAt.Equal(late) {
			s.in.Probes[i].RespondedAt = late.Add(2 * time.Second)
		}
	}
	got := Estimate(s.in, cfg())
	want := late.Add(2 * time.Second)
	if got.UpperKind != "model" || !got.Upper.Equal(want) {
		t.Fatalf("kind %q upper %v, want %v", got.UpperKind, got.Upper, want)
	}
	// A hit answered after the k-th miss also lifts U.
	s = build(goodPre(), goodPost(), 30)
	for i := range s.in.Probes {
		if s.in.Probes[i].SentAt.Equal(s.lastHit.SentAt) {
			s.in.Probes[i].RespondedAt = s.lastHit.SentAt.Add(5 * time.Second)
		}
	}
	got = Estimate(s.in, cfg())
	want = s.lastHit.SentAt.Add(5 * time.Second)
	if got.Upper == nil || !got.Upper.Equal(want) {
		t.Fatalf("upper %v, want %v", got.Upper, want)
	}
}

func TestDirectUpperBeforeLastHitRejected(t *testing.T) {
	s := build(goodPre(), goodPost(), 30)
	d := s.lastHit.SentAt.Add(-time.Second)
	s.in.DirectUpper = &d
	got := Estimate(s.in, cfg())
	if got.UpperKind == "direct" || !got.DirectRejected {
		t.Fatalf("kind %q rejected %v", got.UpperKind, got.DirectRejected)
	}
}

func TestEmptyRateGroupNoModel(t *testing.T) {
	// Last hit is before deletion: no probes in [DeletionStart, L].
	pre := goodPre()
	pre[len(pre)-1] = true
	s := build(pre, make([]bool, 90), 30)
	got := Estimate(s.in, cfg())
	if got.UpperKind != "none" || got.RateComputed {
		t.Fatalf("kind %q rateComputed %v", got.UpperKind, got.RateComputed)
	}
}

func TestEvidenceFlagsExposed(t *testing.T) {
	got := Estimate(build(goodPre(), goodPost(), 30).in, cfg())
	if !got.RunsComputed || !got.RateComputed || got.RunsP <= 0 || got.RateP <= 0 {
		t.Fatalf("%+v", got)
	}
}

func TestInconsistentTiming(t *testing.T) {
	mut := map[string]func(p *Probe){
		"zero responded": func(p *Probe) { p.RespondedAt = time.Time{} },
		"zero sent":      func(p *Probe) { p.SentAt = time.Time{} },
		"resp<sent":      func(p *Probe) { p.RespondedAt = p.SentAt.Add(-time.Millisecond) },
		"ingress<sent":   func(p *Probe) { x := p.SentAt.Add(-time.Millisecond); p.IngressSelectedAt = &x },
		"ingress>resp":   func(p *Probe) { x := p.RespondedAt.Add(time.Millisecond); p.IngressSelectedAt = &x },
	}
	for name, f := range mut {
		s := build(goodPre(), goodPost(), 30)
		f(&s.in.Probes[10])
		got := Estimate(s.in, cfg())
		if got.Valid || got.InvalidReason != "inconsistent_timing" {
			t.Errorf("%s: valid %v reason %q", name, got.Valid, got.InvalidReason)
		}
	}
}

func TestConnectionReuseInvalid(t *testing.T) {
	s := build(goodPre(), goodPost(), 30)
	s.in.FreshConnections = false
	got := Estimate(s.in, cfg())
	if got.Valid || got.InvalidReason != "connection_reuse" {
		t.Fatalf("valid %v reason %q", got.Valid, got.InvalidReason)
	}
}

func TestPostDeletionHitUsesIngressTime(t *testing.T) {
	// Last hit sent before deletion but selected after it.
	pre := goodPre()
	pre[len(pre)-1] = true
	s := build(pre, make([]bool, 90), 30)
	s.in.PerAttemptIngress = true
	d := s.deletion.Add(10 * time.Millisecond)
	s.in.DirectUpper = &d
	sel := s.deletion.Add(time.Millisecond)
	last := len(pre) - 1
	s.in.Probes[last].SentAt = s.deletion.Add(-time.Millisecond)
	s.in.Probes[last].RespondedAt = s.deletion.Add(5 * time.Millisecond)
	s.in.Probes[last].IngressSelectedAt = &sel
	got := Estimate(s.in, cfg())
	if got.InvalidReason == "no_hit_after_deletion" {
		t.Fatalf("ingress-selected-after-deletion hit not counted: %q", got.InvalidReason)
	}
	s.in.PerAttemptIngress = false
	if got := Estimate(s.in, cfg()); got.InvalidReason != "no_hit_after_deletion" {
		t.Fatalf("SentAt mode must not count it: %q", got.InvalidReason)
	}
}
