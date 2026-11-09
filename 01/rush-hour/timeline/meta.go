package timeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TermStartPrecisionMs is the timing margin written with term_start:
// deletionTimestamp is truncated to the second (1000 ms), plus 50 ms for
// the Envoy start_time millisecond truncation and the different points at
// which the API server and Envoy read the (shared, host) clock. The
// collector's receive lag does not enter: term_start comes from the object.
const TermStartPrecisionMs int64 = 1050

// PodMeta is <dir>/app/<uid>.meta.json (contract with winddown.PodMeta);
// the fields after pod_ip are diagnostics.
type PodMeta struct {
	PodUID       string   `json:"pod_uid"`
	PodName      string   `json:"pod_name"`
	Complete     bool     `json:"complete"`
	ExitedAt     *string  `json:"exited_at"`
	TermStart    *string  `json:"term_start"`
	PodIP        string   `json:"pod_ip,omitempty"`
	PrecisionMs  *int64   `json:"term_start_precision_ms,omitempty"`
	CollectStart *string  `json:"collect_start"`
	CollectEnd   *string  `json:"collect_end"`
	StoppedAt    *string  `json:"stopped_at"` // when collect-timeline.sh stop ran (collect_end null: still running then)
	EndMarker    string   `json:"end_marker"`
	Reasons      []string `json:"coverage_reasons"`
}

func rfcp(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

// Finalize writes <uid>.meta.json for every <uid>.log in dir/app, from the
// follower state, the log itself, pods.jsonl and pods-at-stop.json. It is
// safe to run more than once.
func Finalize(dir string) error {
	pods, _, perr := loadPods(dir)
	if perr != nil && !errors.Is(perr, os.ErrNotExist) {
		return perr
	}
	atStop, serr := loadPodsAtStop(dir)
	stoppedAt := readStoppedAt(dir)
	for _, uid := range appLogUIDs(dir) {
		m := buildMeta(dir, uid, pods[uid], atStop, serr == nil)
		m.StoppedAt = stoppedAt
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(dir, DirApp, uid+".meta.json")
		if err := os.WriteFile(path+".tmp", append(b, '\n'), 0o644); err != nil {
			return err
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			return err
		}
	}
	return nil
}

// LoadPodLogMeta assembles the Coverage input for one Pod.
func LoadPodLogMeta(dir, uid string, h *podHist, atStop map[string]*podObj, haveStop bool) (PodLogMeta, []string) {
	m := PodLogMeta{PodUID: uid}
	var notes []string
	var st FollowState
	if b, err := os.ReadFile(filepath.Join(dir, DirApp, uid+".collect.json")); err != nil {
		notes = append(notes, "沒有收集狀態檔（"+uid+".collect.json）")
	} else if err := json.Unmarshal(b, &st); err != nil {
		notes = append(notes, "收集狀態檔無法解析："+err.Error())
	} else {
		m.PodName, m.CollectStart, m.CollectEnd = st.PodName, st.CollectStart, st.CollectEnd
		m.StartupFromBeginning, m.Gaps = st.StartupFromBeginning, st.Gaps
		if st.Ended == "" || st.CollectEnd == nil {
			// The follower died without recording its end: we do not know
			// until when the log was collected.
			notes = append(notes, "log 收集程序沒有正常結束（沒有 collect_end）")
		}
	}
	mk, err := readAppLog(filepath.Join(dir, DirApp, uid+".log"), uid)
	if err != nil {
		notes = append(notes, "log 無法讀取："+err.Error())
		mk = &appMarkers{}
	}
	if mk.Startup != nil {
		m.StartupAt = mk.Startup.At
		if m.StartupAt == nil { // marker present but no usable ts
			m.BadLines++
		}
	}
	m.Graceful = mk.Graceful
	if mk.Sigterm != nil {
		m.SigtermAt = mk.Sigterm.At
	}
	if mk.Exit != nil {
		m.ExitAt = mk.Exit.At
	}
	m.BadLines += mk.Bad
	if h != nil {
		if m.PodName == "" {
			m.PodName = h.Name
		}
		m.FinishedAt = h.FinishedAt
	}
	if p := atStop[uid]; p != nil {
		if c := p.appContainer(); c != nil {
			if c.State.Terminated != nil && c.State.Terminated.FinishedAt != nil {
				m.FinishedAt = c.State.Terminated.FinishedAt
			}
			// Still running at stop and the follower was still on it.
			m.RunningAtStop = c.State.Running != nil && st.Ended == "stopped"
		}
	} else if !haveStop && st.Ended == "stopped" {
		notes = append(notes, "沒有 pods-at-stop.json，不知道停止時 Pod 是否仍在執行")
	}
	return m, notes
}

func buildMeta(dir, uid string, h *podHist, atStop map[string]*podObj, haveStop bool) PodMeta {
	plm, notes := LoadPodLogMeta(dir, uid, h, atStop, haveStop)
	cov := Coverage(plm)
	out := PodMeta{
		PodUID: uid, PodName: plm.PodName, Complete: cov.Complete && len(notes) == 0,
		EndMarker: cov.EndMarker, Reasons: append(cov.Reasons, notes...),
		CollectEnd: rfcp(plm.CollectEnd),
	}
	if !plm.CollectStart.IsZero() {
		out.CollectStart = rfcp(&plm.CollectStart)
	}
	if out.Reasons == nil {
		out.Reasons = []string{}
	}
	if !plm.RunningAtStop {
		out.ExitedAt = rfcp(plm.FinishedAt)
	}
	if h != nil {
		out.PodIP = h.IP
		if est, ok := DeletionStart(h.Obs); ok {
			out.TermStart = rfcp(&est.Start)
			p := TermStartPrecisionMs
			out.PrecisionMs = &p
		}
	}
	if p := atStop[uid]; p != nil && out.PodIP == "" {
		out.PodIP = p.Status.PodIP
	}
	return out
}

// describeCoverage is the one-line Chinese summary used by the timeline.
func describeCoverage(m PodMeta) string {
	if m.Complete {
		return fmt.Sprintf("log 完整（結束依據：%s）", endMarkerText(m.EndMarker))
	}
	return "log 不完整（" + strings.Join(m.Reasons, "；") + "）"
}

func endMarkerText(s string) string {
	switch s {
	case "exit":
		return "程式結束前的最後一行"
	case "sigterm":
		return "收到 SIGTERM 那一行（未開 GRACEFUL_SHUTDOWN）"
	case "running":
		return "量測結束時 Pod 仍在執行"
	}
	return "無"
}

// readStoppedAt reads stopped_at from collect.json (nil when absent).
func readStoppedAt(dir string) *string {
	b, err := os.ReadFile(filepath.Join(dir, "collect.json"))
	if err != nil {
		return nil
	}
	var c struct {
		StoppedAt *string `json:"stopped_at"`
	}
	if json.Unmarshal(b, &c) != nil || c.StoppedAt == nil || parseRFC(c.StoppedAt) == nil {
		return nil
	}
	return c.StoppedAt
}
