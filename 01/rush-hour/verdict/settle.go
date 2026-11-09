package verdict

import "time"

// Transaction outcomes recorded in the App's request_end log line.
const (
	TxCommitted     = "committed"
	TxRolledBack    = "rolled_back"
	TxNotStarted    = "not_started"
	TxCommitUnknown = "commit_unknown"
)

// IngressRecord is what the ingress access log says about one attempt.
type IngressRecord struct {
	// Finished is true when the ingress has logged the end of this attempt.
	Finished bool
	// UpstreamAttempts is how many times the ingress forwarded this attempt
	// to a Pod. nil means the field is missing or could not be collected.
	UpstreamAttempts *int
	// UpstreamPod identifies the Pod the upstream address belongs to; empty
	// when the address is not a Pod's or could not be mapped. Used by Cause.
	UpstreamPod string
	// IngressStart is when the ingress started handling this attempt; nil
	// when unknown. Used by Cause.
	IngressStart *time.Time
}

// AppEnd is the App's "request end" log line for one attempt.
type AppEnd struct {
	TxOutcome    string
	CommitSentAt time.Time // meaningful only when TxOutcome is commit_unknown
	// WriteErr is true when writing or flushing the response returned an error.
	WriteErr bool
	// CtxCanceled is true when the request context was canceled.
	CtxCanceled bool
}

// AttemptEvidence is everything collected about one attempt (one
// X-Koupa-Attempt value).
type AttemptEvidence struct {
	// FullSuccess: Rush Hour received a complete success response.
	FullSuccess bool
	// Ingress is nil when the ingress record for this attempt is missing.
	Ingress *IngressRecord
	// AppReceived: the App log has a "received" line for this attempt.
	AppReceived bool
	// AppReceivedAt is that line's time; nil when absent. Used by Cause.
	AppReceivedAt *time.Time
	// AppEnd is nil when the App log has no "request end" line.
	AppEnd *AppEnd
	// PodExitedAt is when the handling Pod's program exited; nil if it has
	// not exited or the time is unknown.
	PodExitedAt *time.Time
	// LogCovered: log collection for the handling Pod is complete (A11), so
	// the absence of a line is meaningful.
	LogCovered bool
	// Contradictory: the collector found records for this attempt that
	// disagree (e.g. duplicate or out-of-order lines, DB vs log mismatch).
	// Used by State.
	Contradictory bool
	// PodTermStart is the estimated termination start of the upstream Pod,
	// Precision the timing margin applied when comparing against it. Used
	// by Cause.
	PodTermStart *time.Time
	Precision    time.Duration
}

// Settlement says whether the server-side outcome of an attempt is final.
type Settlement struct {
	Settled bool
	Reason  string
}

func unsettled(reason string) Settlement { return Settlement{false, reason} }
func settled(reason string) Settlement   { return Settlement{true, reason} }

// upstreamCount returns the ingress upstream attempt count, or ok=false when
// the ingress record is missing, unfinished, or lacks the count.
func upstreamCount(in *IngressRecord) (n int, why string, ok bool) {
	switch {
	case in == nil:
		return 0, "ingress record missing", false
	case !in.Finished:
		return 0, "attempt not finished at ingress", false
	case in.UpstreamAttempts == nil:
		return 0, "upstream attempt count missing", false
	}
	return *in.UpstreamAttempts, "", true
}

// Settle applies A9 step 3: whether the server-side result of this attempt
// is settled at time now, given the database transaction_timeout.
// "More than transaction_timeout" is strict: exactly txTimeout is not enough.
func Settle(e AttemptEvidence, now time.Time, txTimeout time.Duration) Settlement {
	n, why, ok := upstreamCount(e.Ingress)
	if !ok {
		return unsettled(why)
	}
	switch {
	case n == 0:
		return settled("ingress forwarded to no Pod")
	case n >= 2:
		return unsettled("ingress retried upstream; App cannot tell the tries apart")
	}
	// Exactly one upstream attempt.
	if e.FullSuccess {
		return settled("full success response")
	}
	if e.AppEnd != nil {
		switch e.AppEnd.TxOutcome {
		case TxCommitted, TxRolledBack, TxNotStarted:
			return settled("request ended with " + e.AppEnd.TxOutcome)
		case TxCommitUnknown:
			if now.Sub(e.AppEnd.CommitSentAt) > txTimeout {
				return settled("commit_unknown older than transaction_timeout")
			}
			return unsettled("commit_unknown within transaction_timeout")
		}
		return unsettled("request ended with unrecognised outcome " + e.AppEnd.TxOutcome)
	}
	if !e.AppReceived {
		return unsettled("App has no received line; request may still arrive")
	}
	// Received, no request end.
	if !e.LogCovered {
		return unsettled("received without end, but Pod log incomplete")
	}
	if e.PodExitedAt == nil {
		return unsettled("received without end; Pod program not known to have exited")
	}
	if now.Sub(*e.PodExitedAt) > txTimeout {
		return settled("received without end; Pod exited more than transaction_timeout ago")
	}
	return unsettled("received without end; Pod exited within transaction_timeout")
}
