// Package timeline turns the raw sources collected by
// scripts/collect-timeline.sh into one causal timeline (plan A11, design
// 5.4), and decides whether each Pod's App log is complete.
//
// Collector file format: every collected line is prefixed with the
// collector's receive time as Unix seconds with nine decimals and one space
// ("1762671000.123456789 <raw line>"). JSON watch streams are compacted to
// one object per line first. App logs keep kubectl's --timestamps prefix
// after the collector stamp ("<stamp> <kubelet ts> <json>").
package timeline

import (
	"fmt"
	"time"
)

// PodObservation is one observed state of a Pod (from the pods watch).
type PodObservation struct {
	CollectedAt        time.Time
	DeletionTimestamp  *time.Time
	GracePeriodSeconds *int64
}

// DeletionEstimate is the estimated termination start of a Pod.
//
// Start = first observed deletionTimestamp − deletionGracePeriodSeconds.
// deletionTimestamp is serialised with second precision (truncated), so the
// real request lies in [Start, Start+1s); it also cannot be later than the
// first observation, so Upper = min(Start+1s, ObservedAt).
type DeletionEstimate struct {
	Start      time.Time
	ObservedAt time.Time
	Upper      time.Time
}

// DeletionPrecision is the width of the deletionTimestamp truncation.
const DeletionPrecision = time.Second

// DeletionStart uses the first observation that carries a deletion
// timestamp; later observations may show a shortened grace period and are
// ignored. ok=false when the Pod was never seen deleting.
func DeletionStart(obs []PodObservation) (DeletionEstimate, bool) {
	for _, o := range obs {
		if o.DeletionTimestamp == nil {
			continue
		}
		var grace int64
		if o.GracePeriodSeconds != nil {
			grace = *o.GracePeriodSeconds
		}
		start := o.DeletionTimestamp.Add(-time.Duration(grace) * time.Second)
		upper := start.Add(DeletionPrecision)
		if !o.CollectedAt.IsZero() && o.CollectedAt.Before(upper) {
			upper = o.CollectedAt
		}
		return DeletionEstimate{Start: start, ObservedAt: o.CollectedAt, Upper: upper}, true
	}
	return DeletionEstimate{}, false
}

// Gap is one interruption of a Pod's log stream.
type Gap struct {
	After    time.Time `json:"after"`  // kubelet ts of the last line before the break
	Reason   string    `json:"reason"` // stream error, exit status
	Refilled bool      `json:"refilled"`
	Verified bool      `json:"verified"` // the refill's overlap matched line for line
}

// PodLogMeta is everything Coverage needs about one Pod's log collection.
type PodLogMeta struct {
	PodUID, PodName string
	CollectStart    time.Time  // the follower started
	CollectEnd      *time.Time // the follower stopped (nil: unknown)
	StartupAt       *time.Time // the App's startup marker (App ts)
	// StartupFromBeginning: the startup line came from a read that started
	// at the beginning of the container log (kubectl logs without --since).
	StartupFromBeginning bool
	Graceful             *bool // from the startup line; nil = unknown
	SigtermAt, ExitAt    *time.Time
	Gaps                 []Gap
	BadLines             int  // non-JSON or unparsable lines
	RunningAtStop        bool // the container still ran when collection stopped
	FinishedAt           *time.Time
}

// CoverageResult says whether the log covers the Pod's life.
type CoverageResult struct {
	Complete bool
	// EndMarker: "exit" (graceful), "sigterm" (step 0), "running" (still
	// running at stop) or "" (no end evidence).
	EndMarker string
	Reasons   []string // why it is not complete
}

// Coverage applies plan A11 "log 完整性":
//  1. collection started before the startup marker, or a read from the
//     beginning saw it;
//  2. continuous up to the end marker (exit when GRACEFUL_SHUTDOWN, the
//     sigterm line otherwise) or the Pod still running at stop; finishedAt
//     alone is not proof; every interruption must be refilled and verified.
//
// The third rule (the attempt lies within collection) is per attempt and is
// left to the consumer (CollectStart / CollectEnd).
func Coverage(m PodLogMeta) CoverageResult {
	var r CoverageResult
	switch {
	case m.StartupAt == nil:
		r.Reasons = append(r.Reasons, "沒有啟動標記")
	case !m.StartupFromBeginning && !m.CollectStart.Before(*m.StartupAt):
		r.Reasons = append(r.Reasons, "收集開始晚於啟動標記，且不是從頭讀起")
	}
	switch {
	case m.Graceful == nil:
		r.Reasons = append(r.Reasons, "不知道 GRACEFUL_SHUTDOWN 開關（啟動標記沒有記錄）")
	case *m.Graceful && m.ExitAt != nil:
		r.EndMarker = "exit"
	case !*m.Graceful && m.SigtermAt != nil:
		r.EndMarker = "sigterm"
	case m.RunningAtStop:
		r.EndMarker = "running"
	default:
		want := "sigterm"
		if *m.Graceful {
			want = "exit"
		}
		msg := fmt.Sprintf("沒有結束前的標記（%s）", want)
		if m.FinishedAt != nil {
			msg += "；finishedAt 只能補程式結束時間，不能證明 log 完整"
		}
		r.Reasons = append(r.Reasons, msg)
	}
	for _, g := range m.Gaps {
		switch {
		case !g.Refilled:
			r.Reasons = append(r.Reasons, fmt.Sprintf("串流在 %s 之後中斷，缺口沒有補讀（%s）", fmtT(g.After), g.Reason))
		case !g.Verified:
			r.Reasons = append(r.Reasons, fmt.Sprintf("串流在 %s 之後中斷，補讀的缺口無法逐行核對（%s）", fmtT(g.After), g.Reason))
		}
	}
	if m.BadLines > 0 {
		r.Reasons = append(r.Reasons, fmt.Sprintf("%d 行無法解析", m.BadLines))
	}
	r.Complete = len(r.Reasons) == 0
	return r
}

func fmtT(t time.Time) string {
	if t.IsZero() {
		return "開頭"
	}
	return t.UTC().Format("15:04:05.000")
}
