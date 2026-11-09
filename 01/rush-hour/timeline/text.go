package timeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Write writes dir/timeline.jsonl and dir/timeline.txt.
func Write(dir string, evs []Event) error {
	var jl bytes.Buffer
	enc := json.NewEncoder(&jl)
	enc.SetEscapeHTML(false)
	for _, e := range evs {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "timeline.jsonl"), jl.Bytes(), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "timeline.txt"), []byte(Text(dir, evs)), 0o644)
}

func clock(t *time.Time) string {
	if t == nil {
		return "?"
	}
	return t.UTC().Format("15:04:05.000")
}

// when renders the time of an event with its kind of time.
func when(e Event) string {
	switch {
	case e.EventTime != nil:
		s := "事件 " + clock(e.EventTime)
		if e.CollectedAt != nil {
			s += "（收 " + clock(e.CollectedAt) + "）"
		}
		return s
	case e.IntervalStart != nil:
		return "區間 " + clock(e.IntervalStart) + " ～ " + clock(e.IntervalEnd)
	case e.CollectedAt != nil:
		return "收   " + clock(e.CollectedAt)
	}
	return "（沒有時間）"
}

var kindText = map[string]string{
	KindMissing: "缺少", KindPodCreated: "Pod 建立", KindPodReady: "Pod Ready", KindDeletionStart: "開始終止（推估）",
	KindAuditDelete: "API 刪除請求（audit）", KindContainerExit: "容器結束（kubelet）", KindPodDeleted: "Pod 物件刪除",
	KindEndpoint: "EndpointSlice 變化", KindK8sEvent: "Kubernetes event", KindStartup: "App 啟動標記",
	KindSigterm: "收到 SIGTERM（App）", KindExit: "程式結束前最後一行（App）", KindShutdownTO: "收尾逾時（App）",
	KindLogCoverage: "log 完整性", KindIngressFirst: "入口第一筆", KindIngressLast: "入口最後一筆", KindIngressError: "入口錯誤",
	KindHubbleLast: "Hubble 最後一筆", KindHubbleDrop: "Hubble DROPPED", KindEnvoyStats: "Envoy 統計", KindHostPeak: "主機資源",
}

func kt(k string) string {
	if s, ok := kindText[k]; ok {
		return s
	}
	return k
}

// Text renders the human timeline.
func Text(dir string, evs []Event) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "時間線：%s\n", dir)
	sb.WriteString("時間一律 UTC。「事件」= 來源自己記的時間（括號內是收集端收到的時間）；「收」= 只有收集端時間；「區間」= 只知道落在這段時間內。\n\n")

	sb.WriteString("缺少的來源：")
	var miss []string
	for _, e := range evs {
		if e.Kind == KindMissing && e.PodUID == "" {
			miss = append(miss, fmt.Sprintf("  - %s：%s", e.Source, e.Detail))
		}
	}
	if len(miss) == 0 {
		sb.WriteString("無\n")
	} else {
		sb.WriteString("\n" + strings.Join(miss, "\n") + "\n")
	}

	// Per terminated Pod: the three points, or 缺少.
	byPod := map[string][]Event{}
	names := map[string]string{}
	term := map[string]bool{}
	hasLog := map[string]bool{}
	for _, e := range evs {
		if e.PodUID == "" {
			continue
		}
		byPod[e.PodUID] = append(byPod[e.PodUID], e)
		if e.PodName != "" {
			names[e.PodUID] = e.PodName
		}
		switch e.Kind {
		case KindDeletionStart, KindSigterm, KindExit, KindPodDeleted:
			// (container_finished alone is a completed Job, not a termination)
			term[e.PodUID] = true
		case KindAuditDelete:
			if !strings.Contains(e.Detail, "kubelet") {
				term[e.PodUID] = true
			}
		}
		if e.Source == "app" && e.Kind != KindMissing {
			hasLog[e.PodUID] = true
		}
	}
	var uids []string
	for u := range term {
		uids = append(uids, u)
	}
	sort.Slice(uids, func(i, j int) bool { return names[uids[i]]+uids[i] < names[uids[j]]+uids[j] })
	sb.WriteString("\n被終止的 Pod（開始終止、SIGTERM、程式結束三點；取不到的標「缺少」，不補造時間）：\n")
	if len(uids) == 0 {
		sb.WriteString("  （沒有）\n")
	}
	for _, u := range uids {
		pe := byPod[u]
		find := func(kind string) *Event {
			for i := range pe {
				if pe[i].Kind == kind {
					if kind == KindAuditDelete && strings.Contains(pe[i].Detail, "kubelet") {
						continue
					}
					return &pe[i]
				}
			}
			return nil
		}
		line := func(label string, e *Event, why string) {
			if e == nil {
				fmt.Fprintf(&sb, "    %-16s 缺少（%s）\n", label, why)
				return
			}
			fmt.Fprintf(&sb, "    %-16s %s  [%s]\n", label, when(*e), e.Source)
		}
		fmt.Fprintf(&sb, "  %s（%s）\n", names[u], u)
		line("開始終止（推估）", find(KindDeletionStart), "pods 觀察裡沒有 deletionTimestamp")
		if a := find(KindAuditDelete); a != nil {
			line("刪除請求（audit）", a, "")
		}
		noLog := "沒有這個 Pod 的 App log"
		if hasLog[u] {
			noLog = "App log 沒有這一行"
		}
		line("SIGTERM", find(KindSigterm), noLog)
		exit := find(KindExit)
		kub := find(KindContainerExit)
		switch {
		case exit != nil:
			line("程式結束", exit, "")
			if kub != nil {
				line("  kubelet", kub, "")
			}
		case kub != nil:
			line("程式結束", kub, "")
		default:
			line("程式結束", nil, "沒有 exit 行，也沒有觀察到 kubelet finishedAt")
		}
		if e := find(KindIngressLast); e != nil {
			line("入口最後一筆", e, "")
		}
		if e := find(KindLogCoverage); e != nil {
			fmt.Fprintf(&sb, "    %s\n", e.Detail)
		} else {
			fmt.Fprintf(&sb, "    log 完整性：%s\n", noLog)
		}
	}

	sb.WriteString("\n全部事件（依時間排序）：\n")
	for _, e := range evs {
		pod := e.PodName
		if pod == "" && e.PodUID != "" {
			pod = e.PodUID
		}
		fmt.Fprintf(&sb, "  %-44s %-15s %-22s %-26s %s\n", when(e), e.Source, kt(e.Kind), pod, e.Detail)
	}
	return sb.String()
}
