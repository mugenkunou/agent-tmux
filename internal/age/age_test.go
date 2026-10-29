package age

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "30d", want: 30 * 24 * time.Hour},
		{in: "12h", want: 12 * time.Hour},
		{in: "2w", want: 14 * 24 * time.Hour},
		{in: "30", want: 30 * 24 * time.Hour}, // a bare integer is days
		{in: " 7D ", want: 7 * 24 * time.Hour},
		{in: "1h", want: time.Hour},
		{in: "", bad: true},
		{in: "0", bad: true},
		{in: "0d", bad: true},
		{in: "-3d", bad: true},
		{in: "-5", bad: true},
		{in: "d", bad: true},
		{in: "1.5d", bad: true},
		{in: "30m", bad: true}, // minutes are not an age unit
		{in: "ten", bad: true},
		{in: "99999999999999w", bad: true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("Parse(%q) = %v, want an error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Parse(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
}
