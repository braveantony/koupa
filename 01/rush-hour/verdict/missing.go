package verdict

import "sort"

// ReadyRow is one ready_seq the database assigned in this run (the caller
// filters to the run's client_ref prefix).
type ReadyRow struct {
	Seq     int64
	OrderID int64
}

// BoardSeq is what one board client saw.
type BoardSeq struct {
	Name string
	// Start is the board's first starting point; nil when unknown.
	Start *int64
	// Received lists the ready_seq values in arrival order, across reconnects.
	Received []int64
	// CaughtUp: the board's max received seq reached the cursor in time.
	CaughtUp bool
}

// MissingInput is the reconciliation input of A9 step 7 / A14.
type MissingInput struct {
	DBReady []ReadyRow
	Boards  []BoardSeq
	// Cursor is C: the largest ready_seq of this run at the audit.
	Cursor int64
	// UnsettledSeqs are seqs tied to attempts whose server-side result is
	// not settled; they are never counted as confirmed misses.
	UnsettledSeqs map[int64]bool
}

// MissingResult: PerBoard and NotIncreasing have an entry per board (boards
// without a start are left out of PerBoard); Duplicates only lists boards
// that received some seq more than once. Invalid marks the whole run invalid.
type MissingResult struct {
	PerBoard      map[string]Count
	Duplicates    map[string][]int64
	NotIncreasing map[string]bool
	Invalid       bool
}

// Missing reconciles each board against the database. A seq is in a board's
// range when Start < seq <= Cursor. An in-range seq the board did not
// receive is a confirmed miss only if the board caught up and the seq is
// settled; otherwise it is unknown.
func Missing(in MissingInput) MissingResult {
	res := MissingResult{
		PerBoard:      map[string]Count{},
		Duplicates:    map[string][]int64{},
		NotIncreasing: map[string]bool{},
	}
	for _, b := range in.Boards {
		seen := make(map[int64]int, len(b.Received))
		notInc := false
		for i, s := range b.Received {
			seen[s]++
			if i > 0 && s <= b.Received[i-1] {
				notInc = true
			}
		}
		res.NotIncreasing[b.Name] = notInc
		var dups []int64
		for s, n := range seen {
			if n > 1 {
				dups = append(dups, s)
			}
		}
		if len(dups) > 0 {
			sort.Slice(dups, func(i, j int) bool { return dups[i] < dups[j] })
			res.Duplicates[b.Name] = dups
		}

		if b.Start == nil {
			res.Invalid = true
			continue
		}
		var c Count
		for _, r := range in.DBReady {
			if r.Seq <= *b.Start || r.Seq > in.Cursor || seen[r.Seq] > 0 {
				continue
			}
			if b.CaughtUp && !in.UnsettledSeqs[r.Seq] {
				c.Confirmed++
			} else {
				c.Unknown++
			}
		}
		res.PerBoard[b.Name] = c
	}
	return res
}

// Duplicates counts client_refs with more than one order and the extra
// orders beyond the first. orders maps client_ref to its order_ids;
// unsettledRefs are refs with an attempt whose server-side result is not
// settled, so their order count may still grow: a ref with exactly one
// order adds one unknown to both counts, and one already duplicated adds
// one unknown extra order.
func Duplicates(orders map[string][]int64, unsettledRefs map[string]bool) (clientRefs Count, extraOrders Count) {
	for ref, ids := range orders {
		n := len(ids)
		if n >= 2 {
			clientRefs.Confirmed++
			extraOrders.Confirmed += n - 1
		}
		if !unsettledRefs[ref] {
			continue
		}
		switch {
		case n == 1:
			clientRefs.Unknown++
			extraOrders.Unknown++
		case n >= 2:
			extraOrders.Unknown++
		}
	}
	return clientRefs, extraOrders
}
