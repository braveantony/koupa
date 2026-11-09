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

// DuplicateOrder returns a store.PlaceOptions.DuplicateOrder that is true
// for the next n client_refs and logs each. n <= 0 returns nil.
func DuplicateOrder(n int, log *slog.Logger) func(clientRef string) bool {
	if n <= 0 {
		return nil
	}
	var mu sync.Mutex
	left := n
	return func(ref string) bool {
		mu.Lock()
		defer mu.Unlock()
		if left == 0 {
			return false
		}
		left--
		log.Warn(applog.EvMutationDuplicateOrder, "client_ref", ref)
		return true
	}
}
