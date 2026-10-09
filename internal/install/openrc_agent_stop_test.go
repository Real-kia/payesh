//go:build unix

package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// supervise-daemon starts the service shell in its own session and stops it
// by signalling only that shell. The local agent | ingest pipeline must not
// outlive it, or every restart leaves another sampler running.
func TestOpenRCAgentStopEndsLocalPipeline(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	agentPID := filepath.Join(root, "agent.pid")
	ingestPID := filepath.Join(root, "ingest.pid")
	agent := write("agent", "#!/bin/sh\necho $$ > \""+agentPID+"\"\nexec sleep 300\n")
	ingest := write("ingest", "#!/bin/sh\necho $$ > \""+ingestPID+"\"\nexec cat >/dev/null\n")
	config := write("env", "PAYESH_TRANSPORT_URL=''\n")

	body, ok := serviceDefinition("openrc", "payesh-agent", "127.0.0.1:0")
	if !ok {
		t.Fatal("missing service")
	}
	body = strings.ReplaceAll(body, "/etc/payesh/payesh.env", config)
	body = strings.ReplaceAll(body, "/usr/bin/payesh-agent", agent)
	body = strings.ReplaceAll(body, "/usr/bin/payesh ingest", ingest)
	// Alpine uses ash with pipefail; bash supplies equivalent shell behavior here.
	body = strings.ReplaceAll(body, "command=\"/bin/sh\"", "command=\"/bin/bash\"")
	script := write("service", body)

	shell := exec.Command("/bin/bash", "-c", ". \"$1\"; eval \"set -- $command_args\"; exec \"$command\" \"$@\"", "test", script)
	shell.Env = append(os.Environ(), "PAYESH_TRANSPORT_URL=")
	shell.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := shell.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-shell.Process.Pid, syscall.SIGKILL) })

	pids := make([]int, 0, 2)
	for _, file := range []string{agentPID, ingestPID} {
		pid := waitForPID(t, file)
		pids = append(pids, pid)
	}
	if err := shell.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = shell.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range pids {
		for syscall.Kill(pid, 0) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("pipeline process %d survived the service stop", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func waitForPID(t *testing.T, file string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(file)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipeline process did not start (%s)", file)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
