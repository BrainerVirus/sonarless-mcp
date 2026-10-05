package idle

import "testing"

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"0B": 0, "12B": 12, "1.5kB": 1500, "2MiB": 2 << 20, "3.25MB": 3250000, "1GiB": 1 << 30} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parseSize("lots"); err == nil {
		t.Error("garbage accepted")
	}
}
