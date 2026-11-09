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
	reserve, done := DuplicateOrder(2, log)
	if !reserve("r-1") || !reserve("r-2") {
		t.Fatal("first two must reserve")
	}
	if reserve("r-3") {
		t.Fatal("reserved more than n")
	}
	if len(buf.lines()) != 0 {
		t.Fatal("logged before the commit was confirmed")
	}
	done("r-1", true)
	done("r-2", false) // rolled back: gives the count back
	if !reserve("r-4") {
		t.Fatal("count not given back after rollback")
	}
	done("r-4", true)
	if reserve("r-5") {
		t.Fatal("duplicated more than n")
	}
	var refs []string
	for _, l := range buf.lines() {
		if l["event"] == applog.EvMutationDuplicateOrder {
			refs = append(refs, l["client_ref"].(string))
		}
	}
	if len(refs) != 2 || refs[0] != "r-1" || refs[1] != "r-4" {
		t.Fatalf("logged refs = %v", refs)
	}
}

func TestMutationZeroIsNil(t *testing.T) {
	log, _ := newLog()
	r, d := DuplicateOrder(0, log)
	if DropReady(0, log) != nil || r != nil || d != nil {
		t.Fatal("n=0 should return nil")
	}
}
