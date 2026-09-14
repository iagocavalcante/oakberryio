package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseMemoryMB parses a Fly-style memory size string into megabytes, as
// used by `oak scale --memory`: "1gb"/"2GB" -> 1024/2048, "512mb" -> 512, a
// bare integer is already MB. Anything else (empty, negative, or not one of
// these forms) is rejected.
func ParseMemoryMB(s string) (int, error) {
	lower := strings.ToLower(strings.TrimSpace(s))
	if lower == "" {
		return 0, fmt.Errorf("empty memory size")
	}

	numStr, multiplier := lower, 1
	switch {
	case strings.HasSuffix(lower, "gb"):
		numStr, multiplier = strings.TrimSuffix(lower, "gb"), 1024
	case strings.HasSuffix(lower, "mb"):
		numStr, multiplier = strings.TrimSuffix(lower, "mb"), 1
	}

	n, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, fmt.Errorf("invalid memory size %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid memory size %q: must be positive", s)
	}
	return n * multiplier, nil
}
