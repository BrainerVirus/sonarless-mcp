package main

import "testing"

func TestValidToken(t *testing.T) {
	for _, bad := range []string{"", "   ", "work", "abc def ghi jkl mno", "squ_abcdef\x03ghijklmnop", "short-token", "sqa_0123456789abcdef0123456789abcdef01234567", "sqp_0123456789abcdef0123456789abcdef01234567"} {
		if _, err := validToken(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if got, err := validToken("  squ_0123456789abcdef0123456789abcdef01234567\n"); err != nil || got != "squ_0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("rejected a real-looking token: %q %v", got, err)
	}
}
