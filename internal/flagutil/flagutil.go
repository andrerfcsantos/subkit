// Package flagutil holds small helpers for parsing repeatable CLI flag
// values.
package flagutil

import "strings"

// SplitCSV flattens values that may each contain comma-separated entries into
// a single list, trimming whitespace and dropping empty entries.
func SplitCSV(values []string) []string {
	var result []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				result = append(result, part)
			}
		}
	}
	return result
}
