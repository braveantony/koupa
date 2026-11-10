package timeline

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// Collector file names inside the run directory.
const (
	FilePods      = "pods.jsonl"
	FileSlices    = "endpointslices.jsonl"
	FileEvents    = "events.jsonl"
	FileHubble    = "hubble.jsonl"
	FileIngress   = "ingress.log"
	FileHost      = "host.jsonl"
	FileAudit     = "audit.log"
	FileMissing   = "missing.jsonl"
	FileStatsA    = "envoy-stats-start.txt"
	FileStatsB    = "envoy-stats-end.txt"
	FilePodsAtEnd = "pods-at-stop.json"
	DirApp        = "app"
	AppLabel      = "app.kubernetes.io/name"
	AppName       = "koupa"
	AppContainer  = "koupa"
)

// FormatStamp renders the collector stamp: Unix seconds, nine decimals.
func FormatStamp(t time.Time) string {
	return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond())
}

// splitStamp parses a leading collector stamp. ok=false when the line has
// none (rest is then the whole line).
func splitStamp(line []byte) (*time.Time, []byte, bool) {
	sp := bytes.IndexByte(line, ' ')
	if sp <= 0 {
		return nil, line, false
	}
	head := line[:sp]
	dot := bytes.IndexByte(head, '.')
	if dot <= 0 || len(head)-dot-1 < 1 || len(head)-dot-1 > 9 {
		return nil, line, false
	}
	sec, err1 := strconv.ParseInt(string(head[:dot]), 10, 64)
	fr := string(head[dot+1:])
	for len(fr) < 9 {
		fr += "0"
	}
	ns, err2 := strconv.ParseInt(fr, 10, 64)
	if err1 != nil || err2 != nil {
		return nil, line, false
	}
	t := time.Unix(sec, ns).UTC()
	return &t, line[sp+1:], true
}

// scan calls fn for every non-blank line of path.
func scan(path string, fn func(line []byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) > 0 {
			fn(sc.Bytes())
		}
	}
	return sc.Err()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// watchObject unwraps a --output-watch-events line ({"type","object"});
// a bare object is returned with type "".
func watchObject(js []byte) (string, json.RawMessage) {
	var w struct {
		Type   string          `json:"type"`
		Object json.RawMessage `json:"object"`
	}
	if json.Unmarshal(js, &w) == nil && len(w.Object) > 0 {
		return w.Type, w.Object
	}
	return "", js
}

// ---------------------------------------------------------------- pods

type containerStatus struct {
	Name  string `json:"name"`
	State struct {
		Running *struct {
			StartedAt *time.Time `json:"startedAt"`
		} `json:"running"`
		Terminated *struct {
			ExitCode   int        `json:"exitCode"`
			Reason     string     `json:"reason"`
			FinishedAt *time.Time `json:"finishedAt"`
		} `json:"terminated"`
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting"`
	} `json:"state"`
}

type podObj struct {
	Kind     string `json:"kind"`
	Metadata struct {
		UID                        string            `json:"uid"`
		Name                       string            `json:"name"`
		Labels                     map[string]string `json:"labels"`
		CreationTimestamp          *time.Time        `json:"creationTimestamp"`
		DeletionTimestamp          *time.Time        `json:"deletionTimestamp"`
		DeletionGracePeriodSeconds *int64            `json:"deletionGracePeriodSeconds"`
	} `json:"metadata"`
	Status struct {
		Phase      string `json:"phase"`
		PodIP      string `json:"podIP"`
		Conditions []struct {
			Type               string     `json:"type"`
			Status             string     `json:"status"`
			LastTransitionTime *time.Time `json:"lastTransitionTime"`
		} `json:"conditions"`
		ContainerStatuses []containerStatus `json:"containerStatuses"`
	} `json:"status"`
}

// appContainer returns the koupa container's status (or the first one).
func (p *podObj) appContainer() *containerStatus {
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == AppContainer {
			return &p.Status.ContainerStatuses[i]
		}
	}
	if len(p.Status.ContainerStatuses) > 0 {
		return &p.Status.ContainerStatuses[0]
	}
	return nil
}

// podHist is everything observed about one Pod in the pods watch.
type podHist struct {
	UID, Name, IP string
	App           bool
	Created       *time.Time
	ReadyAt       *time.Time // first Ready=True transition seen
	ReadyAtSeen   *time.Time // collector time of that observation
	Obs           []PodObservation
	FinishedAt    *time.Time // last observed terminated.finishedAt (app container)
	FinishedSeen  *time.Time
	ExitDetail    string
	DeletedSeen   *time.Time // watch DELETED (collector time)
	FirstSeen     *time.Time
}

func (h *podHist) apply(collected *time.Time, typ string, p *podObj) {
	if p.Metadata.Name != "" {
		h.Name = p.Metadata.Name
	}
	if p.Status.PodIP != "" {
		h.IP = p.Status.PodIP
	}
	if p.Metadata.Labels[AppLabel] == AppName {
		h.App = true
	}
	if h.FirstSeen == nil {
		h.FirstSeen = collected
	}
	if p.Metadata.CreationTimestamp != nil {
		h.Created = p.Metadata.CreationTimestamp
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" && h.ReadyAt == nil && c.LastTransitionTime != nil {
			h.ReadyAt, h.ReadyAtSeen = c.LastTransitionTime, collected
		}
	}
	o := PodObservation{DeletionTimestamp: p.Metadata.DeletionTimestamp, GracePeriodSeconds: p.Metadata.DeletionGracePeriodSeconds}
	if collected != nil {
		o.CollectedAt = *collected
	}
	h.Obs = append(h.Obs, o)
	if c := p.appContainer(); c != nil && c.State.Terminated != nil && c.State.Terminated.FinishedAt != nil {
		h.FinishedAt, h.FinishedSeen = c.State.Terminated.FinishedAt, collected
		h.ExitDetail = fmt.Sprintf("exitCode=%d reason=%s", c.State.Terminated.ExitCode, c.State.Terminated.Reason)
	}
	if typ == "DELETED" && h.DeletedSeen == nil {
		h.DeletedSeen = collected
	}
}

// loadPods reads pods.jsonl. bad counts lines that did not parse.
func loadPods(dir string) (map[string]*podHist, int, error) {
	out := map[string]*podHist{}
	bad := 0
	err := scan(filepath.Join(dir, FilePods), func(line []byte) {
		collected, js, _ := splitStamp(line)
		typ, obj := watchObject(js)
		var p podObj
		if json.Unmarshal(obj, &p) != nil || p.Metadata.UID == "" {
			bad++
			return
		}
		h := out[p.Metadata.UID]
		if h == nil {
			h = &podHist{UID: p.Metadata.UID}
			out[p.Metadata.UID] = h
		}
		h.apply(collected, typ, &p)
	})
	return out, bad, err
}

// loadPodsAtStop reads the final `kubectl get pods -o json` list.
func loadPodsAtStop(dir string) (map[string]*podObj, error) {
	b, err := os.ReadFile(filepath.Join(dir, FilePodsAtEnd))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []podObj `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	out := map[string]*podObj{}
	for i := range list.Items {
		out[list.Items[i].Metadata.UID] = &list.Items[i]
	}
	return out, nil
}

// ---------------------------------------------------------------- app logs

// appMarkers is what one App log says about the Pod's life.
type appMarkers struct {
	Startup, Sigterm, Exit, ShutdownTimeout *appMark
	Graceful                                *bool
	Bad                                     int
	Lines                                   int
}

type appMark struct {
	At, Collected *time.Time
	Detail        string
}

// readAppLog parses <stamp> <kubelet ts> <json> lines.
func readAppLog(path, uid string) (*appMarkers, error) {
	m := &appMarkers{}
	err := scan(path, func(line []byte) {
		m.Lines++
		collected, _, _ := splitStamp(line)
		i := bytes.IndexByte(line, '{')
		if i < 0 {
			m.Bad++
			return
		}
		var l struct {
			TS       string `json:"ts"`
			Event    string `json:"event"`
			PodUID   string `json:"pod_uid"`
			Graceful *bool  `json:"graceful_shutdown"`
			Signal   string `json:"signal"`
			Phase    string `json:"phase"`
		}
		if json.Unmarshal(line[i:], &l) != nil || (l.PodUID != "" && l.PodUID != uid) {
			m.Bad++
			return
		}
		var ts *time.Time
		if t, err := time.Parse(time.RFC3339Nano, l.TS); err == nil {
			ts = &t
		}
		mk := &appMark{At: ts, Collected: collected}
		switch l.Event {
		case "startup":
			if m.Startup == nil {
				m.Startup, m.Graceful = mk, l.Graceful
			}
		case "sigterm":
			if m.Sigterm == nil {
				mk.Detail = l.Signal
				m.Sigterm = mk
			}
		case "exit":
			m.Exit = mk
		case "shutdown_timeout":
			m.ShutdownTimeout = mk
		}
	})
	return m, err
}

// appLogUIDs lists the <uid>.log files in dir/app.
func appLogUIDs(dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, DirApp, "*.log"))
	var out []string
	for _, f := range files {
		out = append(out, filepath.Base(f[:len(f)-len(".log")]))
	}
	sort.Strings(out)
	return out
}
