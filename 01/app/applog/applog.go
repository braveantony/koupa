// Package applog builds the app's structured JSON logger. Every line carries
// ts (RFC3339Nano), event and pod_uid; the event name is the slog message.
package applog

import (
	"io"
	"log/slog"
	"time"
)

// Event names.
const (
	EvStartup                = "startup"
	EvTxTimeoutCheck         = "tx_timeout_check"
	EvRequestReceived        = "request_received"
	EvRequestEnd             = "request_end"
	EvSigterm                = "sigterm"
	EvExit                   = "exit"
	EvCommitUnknown          = "commit_unknown"
	EvWriteStart             = "write_start"
	EvWriteEnd               = "write_end"
	EvWriteBlocked           = "write_blocked"
	EvMutationDropReady      = "mutation_drop_ready"
	EvMutationDuplicateOrder = "mutation_duplicate_order"
	EvHookPaused             = "hook_paused"
	EvHookReleased           = "hook_released"
	EvShutdownTimeout        = "shutdown_timeout"
	EvProgressAheadOfH       = "progress_ahead_of_h"
	EvKeyCleanup             = "key_cleanup"
	EvListenActive           = "listen_active"
	EvListenLost             = "listen_lost"
	EvBroadcastQueryFailed   = "broadcast_query_failed"
	EvSubscriberOverflow     = "subscriber_overflow"
	EvBadLastEventID         = "bad_last_event_id"
	EvBoardClosed            = "board_closed"
	EvStartupFailed          = "startup_failed"
)

// New returns a JSON logger writing to w. Use the event constant as the
// message: log.Info(applog.EvStartup, "k", v).
func New(w io.Writer, podUID, podName, version string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.TimeKey:
				return slog.String("ts", a.Value.Time().UTC().Format(time.RFC3339Nano))
			case slog.MessageKey:
				return slog.String("event", a.Value.String())
			}
			return a
		},
	})
	return slog.New(h).With("pod_uid", podUID, "pod_name", podName, "version", version)
}
