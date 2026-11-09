// Package verdict holds the pure-function judgement logic of Rush Hour:
// server-side settlement (A9 step 3), the observed state and cause of a
// failed attempt (A10), and missing/duplicate reconciliation (A9 step 7,
// A14). It does no I/O and never reads the clock; callers pass time in.
package verdict

import "fmt"

// Count is a score that may be only partly known. Confirmed items are
// definite; Unknown items lack the evidence to say either way.
type Count struct {
	Confirmed int
	Unknown   int
}

// String prints "N" when nothing is unknown, otherwise
// "已確認 N；另有 M 項未知" so that an unknown is never shown as a bare 0.
func (c Count) String() string { return c.StringUnit("項") }

// StringUnit is String with a custom unit for the unknown part, e.g.
// StringUnit("個叫號") gives "已確認 0；另有 12 個叫號未知".
func (c Count) StringUnit(unit string) string {
	if c.Unknown > 0 {
		return fmt.Sprintf("已確認 %d；另有 %d %s未知", c.Confirmed, c.Unknown, unit)
	}
	return fmt.Sprintf("%d", c.Confirmed)
}
