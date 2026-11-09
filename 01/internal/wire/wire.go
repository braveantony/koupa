// Package wire holds names shared between the koupa service and the load tool.
package wire

import "fmt"

const (
	// HeaderAttempt carries the attempt ID (sent by rush-hour).
	HeaderAttempt = "X-Koupa-Attempt"
	// HeaderPodUID is returned by the app to identify the serving pod.
	HeaderPodUID = "X-Koupa-Pod-UID"
)

// NewAttemptID formats an attempt ID as <runID>-a<seq>-t<try>.
func NewAttemptID(runID string, seq uint64, try int) string {
	return fmt.Sprintf("%s-a%d-t%d", runID, seq, try)
}
