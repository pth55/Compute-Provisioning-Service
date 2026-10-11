package compute

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestQMPStatusHandshake(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "qmp.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintln(conn, `{"QMP":{"version":{"qemu":{"major":8,"minor":2,"micro":0},"package":""},"capabilities":[]}}`)
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case contains(line, "qmp_capabilities"):
				_, _ = fmt.Fprintln(conn, `{"return":{}}`)
			case contains(line, "query-status"):
				_, _ = fmt.Fprintln(conn, `{"return":{"status":"running","running":true}}`)
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, err := qmpStatus(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("expected running, got %q", status)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || index(s, sub) >= 0) }
func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
