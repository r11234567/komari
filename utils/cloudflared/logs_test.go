package cloudflared

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestProcessDrainsBothPipesBeforeReportingExit(t *testing.T) {
	if os.Getenv("KOMARI_TEST_TUNNEL_LOG_CHILD") == "1" {
		fmt.Fprintln(os.Stdout, strings.Repeat("a", 70<<10))
		fmt.Fprint(os.Stdout, "last stdout without newline")
		fmt.Fprint(os.Stderr, "last stderr without newline")
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessDrainsBothPipesBeforeReportingExit$")
	cmd.Env = append(os.Environ(), "KOMARI_TEST_TUNNEL_LOG_CHILD=1")
	m := &manager{maxLogLines: 10, running: true}
	stdout := &tunnelLogWriter{manager: m}
	stderr := &tunnelLogWriter{manager: m, stderr: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	m.waitProcess(cmd, done, stdout, stderr)
	if !slices.Contains(m.logs, "last stdout without newline") || !slices.Contains(m.logs, "last stderr without newline") {
		t.Fatalf("tail output lost (%d lines)", len(m.logs))
	}
	if !strings.HasSuffix(m.logs[0], " [truncated]") || m.logs[len(m.logs)-1] != "cloudflared stopped" {
		t.Fatalf("unexpected process log ordering or line cap (%d lines)", len(m.logs))
	}
}
