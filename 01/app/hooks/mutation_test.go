package hooks

import (
	"testing"

	"koupa/app/applog"
)

func TestDropReadyLogsSeqs(t *testing.T) {
	log, buf := newLog()
	drop := DropReady(2, log)
	// Subscriber 2 is never dropped.
	if drop(2, 1) {
		t.Fatal("dropped for a later subscriber")
	}
	if !drop(FirstSubID, 10) || !drop(FirstSubID, 11) {
		t.Fatal("first two events for the first subscriber must drop")
	}
	if drop(FirstSubID, 12) {
		t.Fatal("dropped more than n")
	}
	var seqs []float64
	for _, l := range buf.lines() {
		if l["event"] == applog.EvMutationDropReady {
			seqs = append(seqs, l["ready_seq"].(float64))
		}
	}
	if len(seqs) != 2 || seqs[0] != 10 || seqs[1] != 11 {
		t.Fatalf("logged seqs = %v", seqs)
	}
}

func TestDuplicateOrderLogsClientRefs(t *testing.T) {
	log, buf := newLog()
	dup := DuplicateOrder(2, log)
	if !dup("r-1") || !dup("r-2") {
		t.Fatal("first two must duplicate")
	}
	if dup("r-3") {
		t.Fatal("duplicated more than n")
	}
	var refs []string
	for _, l := range buf.lines() {
		if l["event"] == applog.EvMutationDuplicateOrder {
			refs = append(refs, l["client_ref"].(string))
		}
	}
	if len(refs) != 2 || refs[0] != "r-1" || refs[1] != "r-2" {
		t.Fatalf("logged refs = %v", refs)
	}
}

func TestMutationZeroIsNil(t *testing.T) {
	log, _ := newLog()
	if DropReady(0, log) != nil || DuplicateOrder(0, log) != nil {
		t.Fatal("n=0 should return nil")
	}
}
