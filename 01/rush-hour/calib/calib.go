// Package calib estimates, from probe timelines, how long after the delete
// request an ingress keeps selecting a terminating Pod (plan A11, calibrate
// mode steps 3-6). It is pure: no I/O and no wall-clock reads.
package calib

import (
	"math"
	"sort"
	"time"
)

// Probe is one GET /api/whoami probe seen from the Rush Hour side.
type Probe struct {
	SentAt      time.Time
	RespondedAt time.Time
	// PodUID is the Pod that answered; empty if there was no response.
	PodUID string
	// IngressSelectedAt is the ingress per-attempt upstream selection time, if known.
	IngressSelectedAt *time.Time
}

// Input is everything needed to estimate one terminated Pod.
type Input struct {
	Probes        []Probe
	TargetPodUID  string // the terminating Pod being estimated
	DeletionStart time.Time
	PreStopEnd    time.Time
	// DirectUpper is U from endpoint/config update records, nil when unknown.
	DirectUpper       *time.Time
	PerAttemptIngress bool
	// FreshConnections records that every probe used a new client connection.
	FreshConnections bool
}

// Config holds the pre-registered thresholds.
type Config struct {
	MinPreSamples int           // 100
	Alpha         float64       // 0.05
	MissProb      float64       // 0.001
	PreWindow     time.Duration // 10s
	Margin        time.Duration // 5s
}

// DefaultConfig returns the values fixed by plan A11.
func DefaultConfig() Config {
	return Config{MinPreSamples: 100, Alpha: 0.05, MissProb: 0.001,
		PreWindow: 10 * time.Second, Margin: 5 * time.Second}
}

// Upper bound kinds.
const (
	UpperDirect = "direct"
	UpperModel  = "model"
	UpperNone   = "none"
)

// InvalidReason codes.
const (
	ReasonNoHitAfterDeletion = "no_hit_after_deletion"
	ReasonNoUpperBound       = "no_upper_bound"
	ReasonUpperTooClose      = "upper_too_close_to_prestop_end"
	ReasonInconsistentTiming = "inconsistent_timing"
	ReasonConnectionReuse    = "connection_reuse"
)

// Sample is the estimate for one terminated Pod.
type Sample struct {
	DeletionStart  time.Time // copied from Input so Candidate can compute U - deletion
	Lower          time.Time // L; zero when the Pod was never hit
	Upper          *time.Time
	UpperKind      string // direct | model | none
	P              float64
	K              int
	N              int     // responded probes in the pre-deletion window
	RunsZ          float64 // runs-test z; 0 when the test could not be computed
	RunsP          float64 // runs-test p-value; valid only if RunsComputed
	RunsComputed   bool
	RateP          float64 // pre/post hit-rate two-proportion p-value; valid only if RateComputed
	RateComputed   bool
	DirectRejected bool // direct evidence was earlier than an observed hit
	Valid          bool
	InvalidReason  string
}

// Estimate applies A11 steps 3-5 to one Pod.
//
// Probes without a response (empty PodUID) say nothing about selection and are
// excluded from every hit / non-hit count.
func Estimate(in Input, cfg Config) Sample {
	s := Sample{DeletionStart: in.DeletionStart, UpperKind: UpperNone}

	probes := make([]Probe, 0, len(in.Probes))
	badTiming := false
	for _, p := range in.Probes {
		if p.PodUID == "" {
			continue
		}
		if p.SentAt.IsZero() || p.RespondedAt.IsZero() || p.RespondedAt.Before(p.SentAt) ||
			(p.IngressSelectedAt != nil &&
				(p.IngressSelectedAt.Before(p.SentAt) || p.IngressSelectedAt.After(p.RespondedAt))) {
			badTiming = true
		}
		probes = append(probes, p)
	}
	sort.SliceStable(probes, func(i, j int) bool { return probes[i].SentAt.Before(probes[j].SentAt) })
	hit := func(p Probe) bool { return p.PodUID == in.TargetPodUID }

	// Lower bound L: never the App receive time.
	hitAfterDeletion := false
	var hitBound time.Time // latest selection or response time of any hit
	for _, p := range probes {
		if !hit(p) {
			continue
		}
		l := p.SentAt
		if in.PerAttemptIngress && p.IngressSelectedAt != nil {
			l = *p.IngressSelectedAt
		}
		if l.After(s.Lower) {
			s.Lower = l
		}
		if l.After(in.DeletionStart) {
			hitAfterDeletion = true
		}
		if l.After(hitBound) {
			hitBound = l
		}
		if p.RespondedAt.After(hitBound) {
			hitBound = p.RespondedAt
		}
	}

	// Pre-deletion hit sequence, in send order.
	winStart := in.DeletionStart.Add(-cfg.PreWindow)
	var pre []bool
	preHits := 0
	for _, p := range probes {
		if !p.SentAt.Before(winStart) && p.SentAt.Before(in.DeletionStart) {
			h := hit(p)
			pre = append(pre, h)
			if h {
				preHits++
			}
		}
	}
	s.N = len(pre)
	if s.N > 0 {
		s.P = float64(preHits) / float64(s.N)
	}

	if badTiming {
		s.InvalidReason = ReasonInconsistentTiming
		return s
	}
	if in.DirectUpper != nil && in.DirectUpper.Before(hitBound) {
		s.DirectRejected = true // contradicted by an observed hit
	}
	if in.DirectUpper != nil && !s.DirectRejected {
		u := *in.DirectUpper
		s.Upper, s.UpperKind = &u, UpperDirect
	} else if !s.Lower.IsZero() {
		s.K, s.Upper = modelUpper(probes, hit, in, cfg, &s, pre, preHits)
		if s.Upper != nil {
			s.UpperKind = UpperModel
		}
	}

	switch {
	case !in.FreshConnections:
		s.InvalidReason = ReasonConnectionReuse
	case !hitAfterDeletion:
		s.InvalidReason = ReasonNoHitAfterDeletion
	case s.Upper == nil:
		s.InvalidReason = ReasonNoUpperBound
	case s.Upper.After(in.PreStopEnd.Add(-cfg.Margin)):
		s.InvalidReason = ReasonUpperTooClose
	default:
		s.Valid = true
	}
	return s
}

// modelUpper returns k and U under the independent-selection model, or (0, nil)
// when the model must not be used.
func modelUpper(probes []Probe, hit func(Probe) bool, in Input, cfg Config,
	s *Sample, pre []bool, preHits int) (int, *time.Time) {
	if s.N < cfg.MinPreSamples || preHits == 0 {
		return 0, nil
	}
	z, rp := RunsTest(pre)
	if math.IsNaN(z) || math.IsNaN(rp) {
		return 0, nil // cannot be tested -> model not used
	}
	s.RunsZ, s.RunsP, s.RunsComputed = z, rp, true
	if rp < cfg.Alpha {
		return 0, nil
	}
	// Hit rate in [DeletionStart, L] versus the pre-deletion rate.
	postN, postHits := 0, 0
	for _, p := range probes {
		if !p.SentAt.Before(in.DeletionStart) && !p.SentAt.After(s.Lower) {
			postN++
			if hit(p) {
				postHits++
			}
		}
	}
	pp, ok := twoProportionP(preHits, s.N, postHits, postN)
	if !ok {
		return 0, nil // cannot compare rates -> model not used
	}
	s.RateP, s.RateComputed = pp, true
	if pp < cfg.Alpha {
		return 0, nil
	}
	k := minK(s.P, cfg.MissProb)
	// k-th consecutive non-hit probe strictly after L.
	run := 0
	for _, p := range probes {
		if !p.SentAt.After(s.Lower) {
			continue
		}
		if hit(p) {
			run = 0
			continue
		}
		run++
		if run == k {
			// Conservative: no probe sent up to the k-th miss, and no hit, may
			// still be unanswered at U (overlapping probes).
			u := p.RespondedAt
			for _, q := range probes {
				if !q.SentAt.After(p.SentAt) && q.RespondedAt.After(u) {
					u = q.RespondedAt
				}
				if hit(q) && q.RespondedAt.After(u) {
					u = q.RespondedAt
				}
			}
			return k, &u
		}
	}
	return k, nil
}

// minK is the smallest k >= 1 with (1-p)^k <= miss.
func minK(p, miss float64) int {
	q := 1 - p
	if q <= 0 {
		return 1
	}
	k, v := 1, q
	for v > miss && k < 1_000_000 {
		k++
		v *= q
	}
	return k
}

// Candidate returns max(U - DeletionStart) in seconds over valid samples, with
// no buffer added. It needs at least 5 valid samples.
func Candidate(samples []Sample) (seconds float64, ok bool) {
	n := 0
	for _, s := range samples {
		if !s.Valid || s.Upper == nil {
			continue
		}
		n++
		if d := s.Upper.Sub(s.DeletionStart).Seconds(); n == 1 || d > seconds {
			seconds = d
		}
	}
	if n < 5 {
		return 0, false
	}
	return seconds, true
}
