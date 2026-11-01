package age

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"7d", 7 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"36h", 36 * time.Hour},
		{"90m", 90 * time.Minute},
		{"30", 30 * 24 * time.Hour},
		{"1.5d", 36 * time.Hour},
		{"0", 0},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("Parse(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"", "-1d", "abc", "3days", "1.2.3d"} {
		if _, err := Parse(in); err == nil {
			t.Fatalf("Parse(%q) should fail", in)
		}
	}
}

func TestUnmarshalText(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("30d")); err != nil {
		t.Fatal(err)
	}
	if d.Duration != 30*24*time.Hour {
		t.Fatalf("got %v", d.Duration)
	}
}
