package main

import (
	"database/sql"
	"flag"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
)

// An optional MariaDB distribution makes the SQL integration test self-contained.
var mariaBase = flag.String("mariadb-basedir", "", "initialize a disposable SQL test instance using this MariaDB installation")

func disposableDB(t *testing.T) mapping {
	t.Helper()
	dir := t.TempDir()
	run := func(binary string, args ...string) *exec.Cmd {
		cmd := exec.Command(filepath.Join(*mariaBase, "bin", binary), args...)
		cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+filepath.Join(*mariaBase, "lib")+":"+os.Getenv("LD_LIBRARY_PATH"))
		return cmd
	}
	args := []string{"--no-defaults", "--basedir=" + *mariaBase, "--datadir=" + filepath.Join(dir, "data")}
	install := run("mariadb-install-db", append(args, "--auth-root-authentication-method=socket")...)
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("initialize MariaDB: %v\n%s", err, out)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	listener.Close()
	initPath := filepath.Join(dir, "init.sql")
	initSQL := "CREATE DATABASE AA;\nCREATE USER 'app'@'127.0.0.1' IDENTIFIED BY 'local-test';\nGRANT ALL ON AA.* TO 'app'@'127.0.0.1';\n"
	if err := os.WriteFile(initPath, []byte(initSQL), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "database.log")
	cmd := run("mariadbd", append(args, "--bind-address=127.0.0.1", "--port="+port, "--skip-name-resolve", "--socket="+filepath.Join(dir, "db.sock"), "--pid-file="+filepath.Join(dir, "pid"), "--init-file="+initPath, "--log-error="+logPath)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Signal(syscall.SIGTERM); cmd.Wait() })
	cfg := driver.NewConfig()
	cfg.Net, cfg.Addr, cfg.User, cfg.Passwd, cfg.DBName = "tcp", addr, "app", "local-test", "AA"
	connector, err := driver.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); {
		if db.Ping() == nil {
			return mapping{Address: addr, Database: "AA", account: account{User: "app"}, password: "local-test"}
		}
		time.Sleep(100 * time.Millisecond)
	}
	out, _ := os.ReadFile(logPath)
	t.Fatalf("MariaDB failed to start: %s", out)
	return mapping{}
}
