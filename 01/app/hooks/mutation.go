package hooks

import (
	"log/slog"
	"sync"

	"koupa/app/applog"
)

// FirstSubID is the id the broadcaster gives its first registered
// subscriber (ids count up from 1), the one drop-ready targets.
const FirstSubID uint64 = 1

// DropReady returns a broadcast.DropFunc that drops the next n events for
// the first registered subscriber and logs each with its ready_seq. n <= 0
// returns nil (no mutation).
func DropReady(n int, log *slog.Logger) func(subID uint64, seq int64) bool {
	if n <= 0 {
		return nil
	}
	var mu sync.Mutex
	left := n
	return func(subID uint64, seq int64) bool {
		if subID != FirstSubID {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if left == 0 {
			return false
		}
		left--
		log.Warn(applog.EvMutationDropReady, "ready_seq", seq, "sub_id", subID)
		return true
	}
}

// Transaction outcomes DuplicateOrder's done callback acts on (the values of
// store.TxCommitted and store.TxCommitUnknown).
const (
	OutcomeCommitted     = "committed"
	OutcomeCommitUnknown = "commit_unknown"
)

// DuplicateOrder returns the store.PlaceOptions pair for duplicate-order=n.
// reserve is true for the next n client_refs and takes one from the count;
// done settles it: committed or commit_unknown logs the injection, otherwise the
// count is given back. Both are nil when n <= 0. Safe for concurrent use.
func DuplicateOrder(n int, log *slog.Logger) (reserve func(clientRef string) bool, done func(clientRef, outcome string)) {
	if n <= 0 {
		return nil, nil
	}
	var mu sync.Mutex
	left := n
	reserve = func(string) bool {
		mu.Lock()
		defer mu.Unlock()
		if left == 0 {
			return false
		}
		left--
		return true
	}
	done = func(ref, outcome string) {
		// A commit_unknown transaction may have committed the duplicate, so
		// it counts as injected; only a definite non-commit gives it back.
		if outcome == OutcomeCommitted || outcome == OutcomeCommitUnknown {
			log.Warn(applog.EvMutationDuplicateOrder, "client_ref", ref, "outcome", outcome)
			return
		}
		mu.Lock()
		left++
		mu.Unlock()
	}
	return reserve, done
}
