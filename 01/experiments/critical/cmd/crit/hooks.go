package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ------------------------------------------------------------------ hooks

type hooks []string

func parseHooks(s string) hooks {
	var h hooks
	for _, a := range strings.Split(s, ",") {
		if a = strings.TrimSpace(a); a != "" {
			h = append(h, a)
		}
	}
	return h
}

var ctl = &http.Client{Timeout: 10 * time.Second}

func (h hooks) post(name, action, query string) error {
	if len(h) == 0 {
		return errors.New("no hook addresses")
	}
	// Every address is tried even after a failure (a deleted Pod's
	// port-forward may be gone while the others still hold pauses); the
	// errors are returned together.
	var errs []error
	for _, a := range h {
		resp, err := ctl.Post("http://"+a+"/hooks/"+name+"/"+action+query, "", nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s on %s: %w", action, name, a, err))
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			errs = append(errs, fmt.Errorf("%s %s on %s: HTTP %d", action, name, a, resp.StatusCode))
		}
	}
	return errors.Join(errs...)
}

func (h hooks) arm(name string, hold bool) error {
	q := ""
	if hold {
		q = "?hold=1"
	}
	step("arm %s%s on %d Pod(s)", name, q, len(h))
	return h.post(name, "arm", q)
}

func (h hooks) release(name string) error {
	step("release %s on %d Pod(s)", name, len(h))
	return h.post(name, "release", "")
}

// Report is a wait reply: some request is paused at the point.
type Report struct {
	PodUID  string `json:"pod_uid"`
	PodName string `json:"pod_name"`
	Attempt string `json:"attempt"`
	At      string `json:"at"`
}

// wait polls every Pod's wait endpoint until one reports a pause.
func (h hooks) wait(name string, timeout time.Duration) (Report, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, a := range h {
			resp, err := ctl.Get("http://" + a + "/hooks/" + name + "/wait?timeout=300ms")
			if err != nil {
				continue // a Pod being deleted drops its port-forward
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				var r Report
				if err := json.Unmarshal(body, &r); err != nil {
					return r, err
				}
				r.At = now()
				step("pause reported: %s attempt %s", r.PodName, r.Attempt)
				return r, nil
			}
		}
	}
	return Report{}, fmt.Errorf("no pause at %s within %s", name, timeout)
}

func cmdHook(cmd string, args []string) (any, error) {
	if len(args) < 1 {
		return nil, errors.New("hook name required")
	}
	name := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	hk := fs.String("hooks", "", "hook addresses")
	hold := fs.Bool("hold", false, "arm with hold=1")
	timeout := fs.Duration("timeout", 30*time.Second, "wait timeout")
	if err := fs.Parse(args[1:]); err != nil {
		return nil, err
	}
	h := parseHooks(*hk)
	switch cmd {
	case "arm":
		return nil, h.arm(name, *hold)
	case "release":
		return nil, h.release(name)
	}
	r, err := h.wait(name, *timeout)
	if err != nil {
		return nil, err
	}
	return r, nil
}
