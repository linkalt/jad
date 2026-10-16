package main

import (
	"bytes"
	"log/slog"
	"net"
	"strings"
	"testing"
)

func TestWarnIfExposed(t *testing.T) {
	cases := []struct {
		addr string
		warn bool
	}{
		{"127.0.0.1:8788", false},
		{"[::1]:8788", false},
		{"0.0.0.0:8788", true},
		{"[::]:8788", true},
		{"192.168.1.5:8788", true},
	}
	for _, tc := range cases {
		addr, err := net.ResolveTCPAddr("tcp", tc.addr)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		warnIfExposed(slog.New(slog.NewTextHandler(&out, nil)), addr)
		if got := strings.Contains(out.String(), "non-loopback"); got != tc.warn {
			t.Errorf("%s: warned=%v, want %v (%q)", tc.addr, got, tc.warn, out.String())
		}
	}
}
