package timeline

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Event kinds.
const (
	KindMissing        = "missing"
	KindPodCreated     = "pod_created"
	KindPodReady       = "pod_ready"
	KindDeletionStart  = "deletion_start" // estimated termination start (interval)
	KindAuditDelete    = "audit_pod_delete"
	KindContainerExit  = "container_finished" // kubelet finishedAt (interval, second precision)
	KindPodDeleted     = "pod_deleted"
	KindEndpoint       = "endpoint_change"
	KindK8sEvent       = "k8s_event"
	KindStartup        = "startup"
	KindSigterm        = "sigterm"
	KindExit           = "exit"
	KindShutdownTO     = "shutdown_timeout"
	KindLogCoverage    = "log_coverage"
	KindIngressFirst   = "ingress_first_to_pod"
	KindIngressLast    = "ingress_last_to_pod"
	KindIngressError   = "ingress_error"
	KindHubbleLast     = "hubble_last_flow_to_pod"
	KindHubbleDrop     = "hubble_drop"
	KindEnvoyStats     = "envoy_stats_delta"
	KindHostPeak       = "host_peak"
	maxListedPerSource = 200
)

// Event is one timeline entry. EventTime is the source's own time (nil when
// the source has none); CollectedAt is when the collector received it.
// Events known only within an interval have EventTime nil and
// IntervalStart/IntervalEnd set.
type Event struct {
	Source        string     `json:"source"`
	Kind          string     `json:"kind"`
	EventTime     *time.Time `json:"event_time"`
	CollectedAt   *time.Time `json:"collected_at"`
	PodUID        string     `json:"pod_uid,omitempty"`
	PodName       string     `json:"pod_name,omitempty"`
	IntervalStart *time.Time `json:"interval_start,omitempty"`
	IntervalEnd   *time.Time `json:"interval_end,omitempty"`
	Detail        string     `json:"detail,omitempty"`
}

// sortKey: event time, else the interval start, else the collector time.
func (e Event) sortKey() (time.Time, bool) {
	switch {
	case e.EventTime != nil:
		return *e.EventTime, true
	case e.IntervalStart != nil:
		return *e.IntervalStart, true
	case e.CollectedAt != nil:
		return *e.CollectedAt, true
	}
	return time.Time{}, false
}

type builder struct {
	dir     string
	evs     []Event
	pods    map[string]*podHist
	byIP    map[string]*podHist
	byName  map[string]*podHist
	missing map[string]bool
}

func (b *builder) add(e Event) {
	if e.PodUID != "" && e.PodName == "" {
		if h := b.pods[e.PodUID]; h != nil {
			e.PodName = h.Name
		}
	}
	b.evs = append(b.evs, e)
}

func (b *builder) markMissing(source, reason string) {
	b.missing[source] = true
	b.add(Event{Source: source, Kind: KindMissing, Detail: reason})
}

// Build merges every source in dir into one sorted event list.
func Build(dir string) ([]Event, error) {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", dir)
	}
	b := &builder{dir: dir, byIP: map[string]*podHist{}, byName: map[string]*podHist{}, missing: map[string]bool{}}
	// Missing entries the collector wrote.
	_ = scan(filepath.Join(dir, FileMissing), func(line []byte) {
		var m struct{ Source, Agent, Reason, At string }
		if json.Unmarshal(line, &m) == nil && m.Source != "" {
			if m.Agent != "" {
				m.Reason = m.Agent + "：" + m.Reason
			}
			b.markMissing(m.Source, m.Reason)
		}
	})
	b.pods = b.loadPods()
	b.slices()
	b.k8sEvents()
	b.audit()
	b.appLogs()
	b.ingress()
	b.hubble()
	b.envoyStats()
	b.host()
	sort.SliceStable(b.evs, func(i, j int) bool {
		ti, oki := b.evs[i].sortKey()
		tj, okj := b.evs[j].sortKey()
		if oki != okj {
			return !oki // untimed (missing) first
		}
		return ti.Before(tj)
	})
	return b.evs, nil
}

// fileOrMissing returns the path when it exists; otherwise marks the
// source missing (unless the collector already did) and returns "".
func (b *builder) fileOrMissing(source, name string) string {
	p := filepath.Join(b.dir, name)
	if exists(p) {
		return p
	}
	if !b.missing[source] {
		b.markMissing(source, "沒有檔案 "+name)
	}
	return ""
}

func addDur(t time.Time, d time.Duration) *time.Time { x := t.Add(d); return &x }

// ---------------------------------------------------------------- pods

func (b *builder) loadPods() map[string]*podHist {
	if b.fileOrMissing("pods", FilePods) == "" {
		return map[string]*podHist{}
	}
	pods, bad, err := loadPods(b.dir)
	if err != nil {
		b.markMissing("pods", "讀取失敗："+err.Error())
	}
	if bad > 0 {
		b.add(Event{Source: "pods", Kind: KindMissing, Detail: fmt.Sprintf("%d 行無法解析", bad)})
	}
	for uid, h := range pods {
		if h.IP != "" {
			b.byIP[h.IP] = h
		}
		b.byName[h.Name] = h
		if h.Created != nil {
			b.evs = append(b.evs, Event{Source: "pods", Kind: KindPodCreated, PodUID: uid, PodName: h.Name, EventTime: h.Created, CollectedAt: h.FirstSeen, Detail: "creationTimestamp（秒精度）"})
		}
		if h.ReadyAt != nil {
			b.evs = append(b.evs, Event{Source: "pods", Kind: KindPodReady, PodUID: uid, PodName: h.Name, CollectedAt: h.ReadyAtSeen,
				IntervalStart: h.ReadyAt, IntervalEnd: minT(addDur(*h.ReadyAt, time.Second), h.ReadyAtSeen), Detail: "Ready 條件 lastTransitionTime（秒精度）"})
		}
		if est, ok := DeletionStart(h.Obs); ok {
			b.evs = append(b.evs, Event{Source: "pods", Kind: KindDeletionStart, PodUID: uid, PodName: h.Name, CollectedAt: &est.ObservedAt,
				IntervalStart: &est.Start, IntervalEnd: &est.Upper,
				Detail: "推估終止起點 = 第一次觀察到的 deletionTimestamp − deletionGracePeriodSeconds（秒精度）"})
		} else if o, ok := FirstDeletion(h.Obs); ok {
			c := o.CollectedAt
			b.evs = append(b.evs, Event{Source: "pods", Kind: KindDeletionStart, PodUID: uid, PodName: h.Name, CollectedAt: &c,
				Detail: "觀察到刪除，但沒有 deletionGracePeriodSeconds：終止起點未知（只知道不晚於收集端看到的時間）"})
		}
		if h.FinishedAt != nil {
			b.evs = append(b.evs, Event{Source: "pods", Kind: KindContainerExit, PodUID: uid, PodName: h.Name, CollectedAt: h.FinishedSeen,
				IntervalStart: h.FinishedAt, IntervalEnd: minT(addDur(*h.FinishedAt, time.Second), h.FinishedSeen), Detail: "kubelet finishedAt（秒精度）" + h.ExitDetail})
		}
		if h.DeletedSeen != nil {
			b.evs = append(b.evs, Event{Source: "pods", Kind: KindPodDeleted, PodUID: uid, PodName: h.Name, CollectedAt: h.DeletedSeen, Detail: "watch DELETED（只有收集端時間）"})
		}
	}
	return pods
}

func minT(a, b *time.Time) *time.Time {
	if b != nil && b.Before(*a) {
		return b
	}
	return a
}

// ---------------------------------------------------------------- endpointslices

func (b *builder) slices() {
	p := b.fileOrMissing("endpointslices", FileSlices)
	if p == "" {
		return
	}
	type epState struct{ cond, addr string }
	state := map[string]map[string]epState{} // slice -> pod uid -> state
	_ = scan(p, func(line []byte) {
		collected, js, _ := splitStamp(line)
		typ, obj := watchObject(js)
		var s struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Endpoints []struct {
				Addresses  []string `json:"addresses"`
				Conditions struct {
					Ready, Serving, Terminating *bool
				} `json:"conditions"`
				TargetRef *struct{ UID, Name string } `json:"targetRef"`
			} `json:"endpoints"`
		}
		if json.Unmarshal(obj, &s) != nil {
			return
		}
		old := state[s.Metadata.Name]
		cur := map[string]epState{}
		if typ != "DELETED" {
			for _, ep := range s.Endpoints {
				key := strings.Join(ep.Addresses, ",")
				if ep.TargetRef != nil {
					key = ep.TargetRef.UID
				}
				cur[key] = epState{cond: fmt.Sprintf("ready=%s serving=%s terminating=%s", bs(ep.Conditions.Ready), bs(ep.Conditions.Serving), bs(ep.Conditions.Terminating)), addr: strings.Join(ep.Addresses, ",")}
			}
		}
		keys := map[string]bool{}
		for k := range cur {
			keys[k] = true
		}
		for k := range old {
			keys[k] = true
		}
		var sorted []string
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			o, had := old[k]
			c, has := cur[k]
			var detail string
			switch {
			case has && (!had || o.cond != c.cond):
				detail = c.cond + " " + c.addr
			case had && !has:
				detail = "從 EndpointSlice 移除 " + o.addr
			default:
				continue
			}
			if tt := s.Metadata.Annotations["endpoints.kubernetes.io/last-change-trigger-time"]; tt != "" {
				detail += "（last-change-trigger-time " + tt + "，秒精度）"
			}
			e := Event{Source: "endpointslices", Kind: KindEndpoint, CollectedAt: collected, Detail: s.Metadata.Name + ": " + detail}
			if _, ok := b.pods[k]; ok {
				e.PodUID = k
			}
			b.add(e)
		}
		state[s.Metadata.Name] = cur
	})
}

func bs(p *bool) string {
	if p == nil {
		return "?"
	}
	return strconv.FormatBool(*p)
}

// ---------------------------------------------------------------- events

func (b *builder) k8sEvents() {
	p := b.fileOrMissing("events", FileEvents)
	if p == "" {
		return
	}
	seen := map[string]bool{}
	_ = scan(p, func(line []byte) {
		collected, js, _ := splitStamp(line)
		_, obj := watchObject(js)
		var ev struct {
			Metadata struct {
				UID               string     `json:"uid"`
				ResourceVersion   string     `json:"resourceVersion"`
				CreationTimestamp *time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
			EventTime      *time.Time `json:"eventTime"`
			LastTimestamp  *time.Time `json:"lastTimestamp"`
			FirstTimestamp *time.Time `json:"firstTimestamp"`
			Series         *struct {
				LastObservedTime *time.Time `json:"lastObservedTime"`
			} `json:"series"`
			Reason         string                           `json:"reason"`
			Message        string                           `json:"message"`
			Type           string                           `json:"type"`
			InvolvedObject struct{ Kind, Name, UID string } `json:"involvedObject"`
		}
		if json.Unmarshal(obj, &ev) != nil || ev.Reason == "" {
			return
		}
		k := ev.Metadata.UID + "/" + ev.Metadata.ResourceVersion
		if seen[k] {
			return
		}
		seen[k] = true
		t := ev.EventTime
		prec := "微秒"
		switch {
		case ev.Series != nil && ev.Series.LastObservedTime != nil:
			t = ev.Series.LastObservedTime
		case ev.LastTimestamp != nil:
			t, prec = ev.LastTimestamp, "秒"
		case t == nil && ev.FirstTimestamp != nil:
			t, prec = ev.FirstTimestamp, "秒"
		case t == nil:
			t, prec = ev.Metadata.CreationTimestamp, "秒"
		}
		e := Event{Source: "events", Kind: KindK8sEvent, EventTime: t, CollectedAt: collected,
			Detail: fmt.Sprintf("%s %s/%s %s: %s（%s精度）", ev.Type, ev.InvolvedObject.Kind, ev.InvolvedObject.Name, ev.Reason, ev.Message, prec)}
		if ev.InvolvedObject.Kind == "Pod" {
			e.PodUID = ev.InvolvedObject.UID
			e.PodName = ev.InvolvedObject.Name
		}
		b.add(e)
	})
}

// ---------------------------------------------------------------- audit

var auditRe = regexp.MustCompile(`^(\S+) AUDIT: .*method="delete" user="([^"]*)".*uri="/api/v1/namespaces/[^/]+/pods/([^/?"]+)[^"]*" response="(\d+)"`)

// audit reads API server audit lines (legacy format) and keeps Pod deletes
// that are not the kubelet's final delete: the real deletion request.
func (b *builder) audit() {
	p := b.fileOrMissing("audit", FileAudit)
	if p == "" {
		return
	}
	_ = scan(p, func(line []byte) {
		_, rest, _ := splitStamp(line)
		m := auditRe.FindSubmatch(rest)
		if m == nil {
			return
		}
		t, err := time.Parse(time.RFC3339Nano, string(m[1]))
		if err != nil {
			return
		}
		user, name := string(m[2]), string(m[3])
		e := Event{Source: "audit", Kind: KindAuditDelete, EventTime: &t, PodName: name, Detail: fmt.Sprintf("DELETE pods/%s user=%s response=%s", name, user, m[4])}
		if strings.HasPrefix(user, "system:node:") {
			e.Detail += "（kubelet 的最後刪除，不是刪除請求）"
		}
		if h := b.byName[name]; h != nil {
			e.PodUID = h.UID
		}
		b.add(e)
	})
}

// ---------------------------------------------------------------- app logs

func (b *builder) appLogs() {
	uids := appLogUIDs(b.dir)
	if len(uids) == 0 {
		if !b.missing["app"] {
			b.markMissing("app", "沒有任何 App log（app/*.log）")
		}
		return
	}
	have := map[string]bool{}
	for _, uid := range uids {
		have[uid] = true
		mk, err := readAppLog(filepath.Join(b.dir, DirApp, uid+".log"), uid)
		if err != nil {
			b.add(Event{Source: "app", Kind: KindMissing, PodUID: uid, Detail: "log 無法讀取：" + err.Error()})
			continue
		}
		for _, x := range []struct {
			kind string
			m    *appMark
		}{{KindStartup, mk.Startup}, {KindSigterm, mk.Sigterm}, {KindExit, mk.Exit}, {KindShutdownTO, mk.ShutdownTimeout}} {
			if x.m != nil {
				b.add(Event{Source: "app", Kind: x.kind, PodUID: uid, EventTime: x.m.At, CollectedAt: x.m.Collected, Detail: x.m.Detail})
			}
		}
		var meta PodMeta
		if raw, err := os.ReadFile(filepath.Join(b.dir, DirApp, uid+".meta.json")); err == nil && json.Unmarshal(raw, &meta) == nil {
			b.add(Event{Source: "app", Kind: KindLogCoverage, PodUID: uid, CollectedAt: parseRFC(meta.CollectEnd), Detail: describeCoverage(meta)})
		} else {
			b.add(Event{Source: "app", Kind: KindLogCoverage, PodUID: uid, Detail: "沒有 meta.json（收集尚未 stop），完整性未判定"})
		}
	}
	for uid, h := range b.pods {
		if h.App && !have[uid] {
			b.add(Event{Source: "app", Kind: KindMissing, PodUID: uid, Detail: "這個 Pod 沒有 App log"})
		}
	}
}

func parseRFC(s *string) *time.Time {
	if s == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil
	}
	return &t
}

// ---------------------------------------------------------------- ingress

func (b *builder) ingress() {
	p := b.fileOrMissing("ingress", FileIngress)
	if p == "" {
		return
	}
	type span struct {
		first, last         time.Time
		firstC, lastC       *time.Time
		n                   int
		firstLine, lastLine string
	}
	spans := map[string]*span{}
	listed, extra := 0, 0
	_ = scan(p, func(line []byte) {
		collected, _, _ := splitStamp(line)
		i := bytes.IndexByte(line, '{')
		if i < 0 {
			return
		}
		var l struct {
			StartTime     string          `json:"start_time"`
			Path          string          `json:"path"`
			Status        json.RawMessage `json:"status"`
			UpstreamHost  string          `json:"upstream_host"`
			ResponseFlags string          `json:"response_flags"`
			Details       string          `json:"response_code_details"`
			Attempts      json.RawMessage `json:"upstream_attempts"`
			Attempt       string          `json:"attempt"`
		}
		if json.Unmarshal(line[i:], &l) != nil {
			return
		}
		t, err := time.Parse(time.RFC3339Nano, l.StartTime)
		if err != nil {
			return
		}
		host, _, _ := net.SplitHostPort(l.UpstreamHost)
		if host != "" {
			s := spans[host]
			if s == nil {
				s = &span{first: t, firstC: collected}
				spans[host] = s
			}
			s.n++
			if t.Before(s.first) {
				s.first, s.firstC = t, collected
			}
			if !t.Before(s.last) {
				s.last, s.lastC = t, collected
			}
		}
		st := strings.Trim(string(l.Status), `"`)
		if !strings.HasPrefix(st, "2") || (l.ResponseFlags != "" && l.ResponseFlags != "-") {
			if listed >= maxListedPerSource {
				extra++
				return
			}
			listed++
			e := Event{Source: "ingress", Kind: KindIngressError, EventTime: &t, CollectedAt: collected,
				Detail: fmt.Sprintf("%s status=%s flags=%s details=%s upstream=%s attempts=%s attempt=%s", l.Path, st, l.ResponseFlags, l.Details, l.UpstreamHost, string(l.Attempts), l.Attempt)}
			if h := b.byIP[host]; h != nil {
				e.PodUID = h.UID
			}
			b.add(e)
		}
	})
	if extra > 0 {
		b.add(Event{Source: "ingress", Kind: KindIngressError, Detail: fmt.Sprintf("另有 %d 筆非 2xx 或有回應旗標的紀錄沒有列出（見 ingress.log）", extra)})
	}
	for host, s := range spans {
		e1 := Event{Source: "ingress", Kind: KindIngressFirst, EventTime: &s.first, CollectedAt: s.firstC, Detail: fmt.Sprintf("第一筆送到 %s（共 %d 筆）", host, s.n)}
		e2 := Event{Source: "ingress", Kind: KindIngressLast, EventTime: addDur(s.last, 0), CollectedAt: s.lastC, Detail: fmt.Sprintf("最後一筆送到 %s（access log 開始時間）", host)}
		if h := b.byIP[host]; h != nil {
			e1.PodUID, e2.PodUID = h.UID, h.UID
		}
		b.add(e1)
		b.add(e2)
	}
}

// ---------------------------------------------------------------- hubble

func (b *builder) hubble() {
	p := b.fileOrMissing("hubble", FileHubble)
	if p == "" {
		return
	}
	last := map[string]*Event{}
	listed, extra := 0, 0
	_ = scan(p, func(line []byte) {
		collected, js, _ := splitStamp(line)
		var f struct {
			Flow struct {
				Time       *time.Time `json:"time"`
				Verdict    string     `json:"verdict"`
				DropReason string     `json:"drop_reason_desc"`
				Source     struct {
					PodName string `json:"pod_name"`
				} `json:"source"`
				Destination struct {
					PodName string `json:"pod_name"`
				} `json:"destination"`
				Summary string `json:"Summary"`
			} `json:"flow"`
		}
		if json.Unmarshal(js, &f) != nil || f.Flow.Time == nil {
			return
		}
		if h := b.byName[f.Flow.Destination.PodName]; h != nil && h.App {
			last[h.UID] = &Event{Source: "hubble", Kind: KindHubbleLast, PodUID: h.UID, EventTime: f.Flow.Time, CollectedAt: collected, Detail: "最後一筆送往這個 Pod 的流量"}
		}
		if f.Flow.Verdict == "DROPPED" {
			if listed >= maxListedPerSource {
				extra++
				return
			}
			listed++
			e := Event{Source: "hubble", Kind: KindHubbleDrop, EventTime: f.Flow.Time, CollectedAt: collected,
				Detail: fmt.Sprintf("%s -> %s DROPPED %s %s", f.Flow.Source.PodName, f.Flow.Destination.PodName, f.Flow.DropReason, f.Flow.Summary)}
			if h := b.byName[f.Flow.Destination.PodName]; h != nil {
				e.PodUID = h.UID
			}
			b.add(e)
		}
	})
	if extra > 0 {
		b.add(Event{Source: "hubble", Kind: KindHubbleDrop, Detail: fmt.Sprintf("另有 %d 筆 DROPPED 沒有列出", extra)})
	}
	for _, e := range last {
		b.add(*e)
	}
}

// ---------------------------------------------------------------- envoy stats

// readStats sums "name{labels} value" lines across nodes; "# ..." lines are
// headers. at is the first header's collected_at.
func readStats(path string) (map[string]float64, *time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	out := map[string]float64{}
	var at *time.Time
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "#") {
			if i := strings.Index(l, "collected_at="); i >= 0 && at == nil {
				t, _, _ := splitStamp([]byte(strings.Fields(l[i+len("collected_at="):])[0] + " x"))
				at = t
			}
			continue
		}
		sp := strings.LastIndexByte(l, ' ')
		if sp < 0 {
			continue
		}
		v, err := strconv.ParseFloat(l[sp+1:], 64)
		if err != nil {
			continue
		}
		out[l[:sp]] += v
	}
	return out, at, sc.Err()
}

var statKeys = map[string]bool{
	"upstream_rq_total": true, "upstream_rq_xx": true, "internal_upstream_rq_xx": true, "upstream_rq_retry": true,
	"upstream_rq_retry_limit_exceeded": true, "upstream_cx_destroy_remote_with_active_rq": true,
	"upstream_rq_timeout": true, "upstream_cx_connect_fail": true, "upstream_rq_pending_failure_eject": true,
}

var codeClassRe = regexp.MustCompile(`envoy_response_code_class="(\d)"`)

func (b *builder) envoyStats() {
	pa := b.fileOrMissing("envoy-stats", FileStatsA)
	pb := ""
	if pa != "" {
		pb = b.fileOrMissing("envoy-stats", FileStatsB)
	}
	if pa == "" || pb == "" {
		return
	}
	sa, _, err1 := readStats(pa)
	sb, at, err2 := readStats(pb)
	if err := errors.Join(err1, err2); err != nil {
		b.markMissing("envoy-stats", err.Error())
		return
	}
	var keys []string
	for k := range sb {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// The A11 counters (all counters stay in the raw files).
	var parts []string
	for _, k := range keys {
		name, labels := k, ""
		if i := strings.IndexByte(k, '{'); i >= 0 {
			name, labels = k[:i], k[i:]
		}
		if !statKeys[strings.TrimPrefix(name, "envoy_cluster_")] {
			continue
		}
		short := strings.TrimPrefix(name, "envoy_cluster_")
		if m := codeClassRe.FindStringSubmatch(labels); m != nil {
			short += "{" + m[1] + "xx}"
		}
		parts = append(parts, fmt.Sprintf("%s %+g", short, sb[k]-sa[k]))
	}
	if len(parts) == 0 {
		parts = []string{"結束時的統計裡沒有 koupa cluster 的計數器"}
	}
	b.add(Event{Source: "envoy-stats", Kind: KindEnvoyStats, CollectedAt: at, Detail: "koupa cluster 量測期間各節點加總差值：" + strings.Join(parts, "；")})
}

// ---------------------------------------------------------------- host

func (b *builder) host() {
	p := b.fileOrMissing("host", FileHost)
	if p == "" {
		return
	}
	var maxCPU, maxMem float64
	var atCPU *time.Time
	n := 0
	_ = scan(p, func(line []byte) {
		var h struct {
			At  string  `json:"at"`
			CPU float64 `json:"cpu_busy_pct"`
			Mem float64 `json:"mem_used_pct"`
		}
		if json.Unmarshal(line, &h) != nil {
			return
		}
		n++
		if h.CPU >= maxCPU {
			maxCPU = h.CPU
			atCPU = parseRFC(&h.At)
		}
		if h.Mem > maxMem {
			maxMem = h.Mem
		}
	})
	if n > 0 {
		b.add(Event{Source: "host", Kind: KindHostPeak, CollectedAt: atCPU, Detail: fmt.Sprintf("主機 CPU 最高 %.1f%%、記憶體最高 %.1f%%（%d 筆）", maxCPU, maxMem, n)})
	}
}
