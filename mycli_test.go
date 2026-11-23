package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMycliLoginWithoutDatabase(t *testing.T) {
	if *mariaBase == "" {
		t.Skip("set -mariadb-basedir to exercise mycli with a disposable database")
	}
	bin, err := exec.LookPath("mycli")
	if err != nil {
		t.Skip("mycli unavailable")
	}
	addr := startGateway(t, newGateway(), disposableDB(t))
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rc := filepath.Join(dir, "myclirc")
	if err := os.WriteFile(rc, []byte("[main]\nlog_file = "+filepath.Join(dir, "mycli.log")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--myclirc", rc, "--ssl-mode=off", "-h", host, "-P", port, "-u", "gateway", "-e", "SET NAMES latin1 COLLATE latin1_bin; SET AUTOCOMMIT = 0; USE A; SELECT CONCAT(DATABASE(), '/', @@collation_connection, '/', @@autocommit + 0)")
	cmd.Env = append(os.Environ(), "MYSQL_PWD=front-secret")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "AA/latin1_bin/0") {
		t.Fatalf("mycli: %v\n%s", err, out)
	}
}

// Exercise the actual completion_refresh thread that mycli's batch mode skips.
func TestMycliCompletionWithoutDatabase(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil || exec.Command(python, "-c", "import mycli").Run() != nil {
		t.Skip("mycli Python module unavailable")
	}
	addr := startGateway(t, newGateway(), mapping{})
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	const script = `
import sys
from queue import Queue
from mycli.completion_refresher import CompletionRefresher
from mycli.sqlexecute import SQLExecute

conn = SQLExecute(None, "gateway", "front-secret", sys.argv[1], int(sys.argv[2]), None, "utf8mb4", False, None)
try:
    assert conn.databases() == ["A", "B"]
    ready = Queue()
    refresher = CompletionRefresher()
    refresher.refresh(conn, ready.put)
    try:
        completer = ready.get(timeout=5)
        assert completer.databases == ["A", "B"], completer.databases
    finally:
        refresher.stop()
finally:
    conn.close()
`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, python, "-c", script, host, port).CombinedOutput(); err != nil {
		t.Fatalf("mycli completion: %v\n%s", err, out)
	}
}
