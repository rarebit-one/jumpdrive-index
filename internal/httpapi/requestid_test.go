package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInboundRequestID(t *testing.T) {
	cases := map[string]string{
		"":                                     "",
		"abc-123":                              "abc-123",
		"0195f3c2-6a0e-7f43-9b0a-1d2e3f405162": "0195f3c2-6a0e-7f43-9b0a-1d2e3f405162",
		"req:a.b_c":                            "req:a.b_c",
		"has space":                            "",
		"inject\r\nX-Evil: 1":                  "",
		strings.Repeat("a", 129):               "",
	}
	for in, want := range cases {
		r := httptest.NewRequest("POST", "/mcp", nil)
		if in != "" {
			r.Header["X-Request-Id"] = []string{in}
		}
		if got := inboundRequestID(r); got != want {
			t.Errorf("inboundRequestID(%q) = %q, want %q", in, got, want)
		}
	}
}
