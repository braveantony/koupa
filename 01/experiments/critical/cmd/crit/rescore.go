package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"

	"koupa/rush-hour/winddown"
)

// cmdRescore recomputes a Rush Hour scorecard from its run directory: the
// facts stored in scorecard.json (same evaluation time) and the evidence
// files as they are now. Used by 06 past-limit after the timeline
// collector has stopped, so every Pod's log coverage is final (A11): an
// attempt still unknown then is unknown because the wind-down limit
// passed, not because a log was incomplete.
func cmdRescore(args []string) (any, error) {
	fs := flag.NewFlagSet("rescore", flag.ContinueOnError)
	dir := fs.String("dir", "", "Rush Hour run directory (--out of rush-hour run)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(*dir, "scorecard.json"))
	if err != nil {
		return nil, err
	}
	var sc struct {
		Facts json.RawMessage `json:"facts"`
	}
	if err := json.Unmarshal(b, &sc); err != nil {
		return nil, err
	}
	tmp := filepath.Join(*dir, ".facts.json")
	if err := os.WriteFile(tmp, sc.Facts, 0o644); err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	f, err := winddown.ReadFacts(tmp)
	if err != nil {
		return nil, err
	}
	ev := winddown.LoadEvidence(winddown.Paths{
		Attempts: filepath.Join(*dir, "attempts.jsonl"),
		Boards:   filepath.Join(*dir, "boards.jsonl"),
		AppLogs:  f.AppLogsDir,
		Ingress:  f.IngressPath,
	})
	return winddown.Evaluate(f, ev), nil
}
