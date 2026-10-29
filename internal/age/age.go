// Package age parses the ages that rm --older-than takes: 30d, 12h, 2w, or a
// bare integer meaning days.
package age

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	day  = 24 * time.Hour
	week = 7 * day
)

// Parse reads an age such as "30d", "12h" or "2w". A bare integer means days.
// The age must be positive.
func Parse(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("age is empty (use 30d, 12h or 2w)")
	}
	unit := day
	num := s
	switch last := s[len(s)-1]; last {
	case 'h', 'H':
		unit, num = time.Hour, s[:len(s)-1]
	case 'd', 'D':
		unit, num = day, s[:len(s)-1]
	case 'w', 'W':
		unit, num = week, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid age %q (use 30d, 12h or 2w; a bare number means days)", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("age must be positive, got %q", s)
	}
	// Reject values that overflow a Duration.
	if n > int64(1<<62)/int64(unit) {
		return 0, fmt.Errorf("age %q is too large", s)
	}
	return time.Duration(n) * unit, nil
}
