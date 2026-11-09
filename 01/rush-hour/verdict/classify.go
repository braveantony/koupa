package verdict

// StateKind is the observed state of a failed attempt (A10). The type is
// not called State because State is the classifying function.
type StateKind string

const (
	ClientGotError        StateKind = "client_got_error"         // 客人收到錯誤回應
	NotReachedApp         StateKind = "not_reached_app"          // 沒送到 App
	ReachedNotEnded       StateKind = "reached_not_ended"        // 送到了，App 沒有結束這個請求
	WriteFailedOrCanceled StateKind = "write_failed_or_canceled" // App 結束請求時寫入失敗或被取消
	WriteFlushOK          StateKind = "write_flush_ok"           // App 回報寫入和 flush 成功
	Unknown               StateKind = "unknown"                  // 未知
)

// State classifies a failed attempt in A10 order; the first match wins.
// gotFullErrorResponse means Rush Hour received a complete 4xx/5xx response.
func State(a AttemptEvidence, gotFullErrorResponse bool) StateKind {
	if gotFullErrorResponse {
		return ClientGotError
	}
	n, _, ok := upstreamCount(a.Ingress)
	if !ok || n >= 2 {
		return Unknown
	}
	if n == 0 {
		return NotReachedApp
	}
	// Exactly one upstream attempt: sub-classify by the App log, which is
	// only trustworthy when collection is complete and consistent.
	if !a.LogCovered || a.Contradictory {
		return Unknown
	}
	switch {
	case !a.AppReceived && a.AppEnd == nil:
		return Unknown // request may still arrive
	case !a.AppReceived:
		return Unknown // request end without received: contradiction
	case a.AppEnd == nil:
		return ReachedNotEnded
	case a.AppEnd.WriteErr || a.AppEnd.CtxCanceled:
		return WriteFailedOrCanceled
	default:
		return WriteFlushOK
	}
}

// CauseKind is the inferred cause of a failed attempt (A10 原因 table).
type CauseKind string

const (
	CauseUnknown                CauseKind = "unknown"
	CauseRoutedAfterTermination CauseKind = "routed_after_termination" // 入口在 Pod 開始終止後仍選了它
	CauseExitedMidRequest       CauseKind = "exited_mid_request"       // 請求處理到一半，程式就結束了
)

// Cause returns a cause only when the A10 table's required evidence is
// complete; otherwise CauseUnknown. state is the result of State for the
// same attempt. When both causes hold, the table's first row wins.
func Cause(a AttemptEvidence, state StateKind) CauseKind {
	if routedAfterTermination(a) {
		return CauseRoutedAfterTermination
	}
	if state == ReachedNotEnded && !a.FullSuccess &&
		a.AppReceivedAt != nil && a.PodExitedAt != nil &&
		a.AppReceivedAt.Before(*a.PodExitedAt) {
		return CauseExitedMidRequest
	}
	return CauseUnknown
}

// routedAfterTermination: exactly one upstream attempt to a known Pod, and
// the ingress start is later than the Pod's termination start even after
// allowing for timing precision.
func routedAfterTermination(a AttemptEvidence) bool {
	n, _, ok := upstreamCount(a.Ingress)
	if !ok || n != 1 || a.Ingress.UpstreamPod == "" ||
		a.Ingress.IngressStart == nil || a.PodTermStart == nil {
		return false
	}
	return a.Ingress.IngressStart.After(a.PodTermStart.Add(a.Precision))
}
