package store

import "fmt"

// FormatNumber renders an order number as a 4-digit zero-padded string.
func FormatNumber(n int64) string {
	return fmt.Sprintf("%04d", n)
}
