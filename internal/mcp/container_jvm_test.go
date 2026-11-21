package mcp

import (
	"testing"
)

func TestJavaToolOptionsEnv(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"-Xmx64m -XX:+UseSerialGC", "JAVA_TOOL_OPTIONS=-Xmx64m -XX:+UseSerialGC"},
		{"none", ""},
		{"", ""},
	} {
		if got := javaToolOptions(tc.value); got != tc.want {
			t.Errorf("%q -> %q, want %q", tc.value, got, tc.want)
		}
	}
}
