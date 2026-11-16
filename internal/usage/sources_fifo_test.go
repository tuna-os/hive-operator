//go:build unix

package usage

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Any agent can create files beside a conversation. A named pipe at the -wal
// path must not hold the collector: opening one for reading waits for a
// writer, and nothing ever writes.
func TestAgentFromDBDoesNotBlockOnNamedPipe(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c1.db"), []byte("no workspace path in here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "c1.db-wal"), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	c := &Collector{}
	got := make(chan string, 1)
	go func() { got <- c.agentFromDB([]string{dir}, "c1") }()
	select {
	case a := <-got:
		if a != Unattributed {
			t.Fatalf("agent = %q, want %q", a, Unattributed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agentFromDB did not return: it is waiting on the named pipe at the -wal path")
	}
}

// The non-blocking open must still read an ordinary -wal file.
func TestAgentFromDBReadsRegularWAL(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c2.db"), []byte("no workspace path in here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c2.db-wal"), []byte("cwd /data/agents/guide/work"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Collector{}
	if got := c.agentFromDB([]string{dir}, "c2"); got != "guide" {
		t.Fatalf("agent = %q, want %q", got, "guide")
	}
}
