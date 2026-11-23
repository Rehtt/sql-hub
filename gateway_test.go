package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	driver "github.com/go-sql-driver/mysql"
)

func listenTest(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return l.Addr().String()
}

type testDB struct {
	server.EmptyHandler
	db string
}

func (h *testDB) UseDB(name string) error { h.db = name; return nil }
func (h *testDB) HandleQuery(q string) (*mysql.Result, error) {
	if q != "SELECT DATABASE()" {
		return nil, mysql.NewDefaultError(mysql.ER_PARSE_ERROR)
	}
	rs, err := mysql.BuildSimpleTextResultset([]string{"DATABASE()"}, [][]any{{h.db}})
	return &mysql.Result{Resultset: rs}, err
}

func targetFixture(t *testing.T) mapping {
	t.Helper()
	srv := server.NewServer("8.0.11", mysql.DEFAULT_COLLATION_ID, mysql.AUTH_NATIVE_PASSWORD, nil, nil)
	auth := server.NewInMemoryAuthenticationHandler(mysql.AUTH_NATIVE_PASSWORD)
	if err := auth.AddUser("app", "target-secret"); err != nil {
		t.Fatal(err)
	}
	addr := listenTest(t, func(raw net.Conn) {
		defer raw.Close()
		c, err := srv.NewCustomizedConn(raw, auth, &testDB{})
		if err != nil {
			return
		}
		for !c.Closed() {
			if c.HandleCommand() != nil {
				return
			}
		}
	})
	return mapping{Address: addr, Database: "AA", account: account{User: "app"}, password: "target-secret"}
}

func startGateway(t *testing.T, g *gateway, r mapping) string {
	t.Helper()
	g.routes = map[string]mapping{"A": r, "B": r}
	if err := g.auth.AddUser("gateway", "front-secret"); err != nil {
		t.Fatal(err)
	}
	return listenTest(t, g.serve)
}

func openDB(t *testing.T, addr, db, pass string) *sql.DB {
	t.Helper()
	cfg := driver.NewConfig()
	cfg.Net, cfg.Addr, cfg.User, cfg.Passwd, cfg.DBName = "tcp", addr, "gateway", pass, db
	cfg.ReadTimeout = 4 * time.Second
	c, err := driver.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pool := sql.OpenDB(c)
	t.Cleanup(func() { pool.Close() })
	return pool
}

func wantCode(t *testing.T, err error, code uint16) {
	t.Helper()
	var my *driver.MySQLError
	if !errors.As(err, &my) || my.Number != code {
		t.Fatalf("want MySQL %d, got %v", code, err)
	}
}

func TestRouting(t *testing.T) {
	r := targetFixture(t)
	addr := startGateway(t, newGateway(), r)
	wantCode(t, openDB(t, addr, "missing", "wrong").Ping(), mysql.ER_ACCESS_DENIED_ERROR)
	wantCode(t, openDB(t, addr, "missing", "front-secret").Ping(), mysql.ER_BAD_DB_ERROR)
	db := openDB(t, addr, "", "front-secret")
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exec := func(q string) error { _, err := conn.ExecContext(context.Background(), q); return err }
	wantCode(t, exec("SELECT DATABASE()"), mysql.ER_NO_DB_ERROR)
	wantCode(t, exec("USE missing"), mysql.ER_BAD_DB_ERROR)
	if err := exec("/* route */ uSe `A` ;"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, exec("USE B"), mysql.ER_NOT_SUPPORTED_YET)
	if err := exec("USE A"); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := conn.QueryRowContext(context.Background(), "SELECT DATABASE()").Scan(&name); err != nil || name != "AA" {
		t.Fatalf("database=%q err=%v", name, err)
	}
	other, err := client.Connect(addr, "gateway", "front-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.UseDB("A"); err != nil {
		t.Fatal(err)
	}
	r.password = "bad"
	wantCode(t, openDB(t, startGateway(t, newGateway(), r), "A", "front-secret").Ping(), mysql.ER_CONNECT_TO_FOREIGN_DATA_SOURCE)
}

func TestBackendDisconnect(t *testing.T) {
	r := targetFixture(t)
	addr := r.Address
	backendConn := make(chan net.Conn, 1)
	r.Address = listenTest(t, func(raw net.Conn) {
		up, err := net.Dial("tcp", addr)
		if err != nil {
			return
		}
		backendConn <- raw
		bridge(raw, up)
	})
	gw := startGateway(t, newGateway(), r)
	c, err := client.Connect(gw, "gateway", "front-secret", "A")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	(<-backendConn).Close()
	c.SetReadDeadline(time.Now().Add(time.Second))
	_, err = c.ReadPacket()
	var timeout net.Error
	if err == nil || errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("expected backend closure to interrupt idle client: %v", err)
	}
}

// Run against a disposable MySQL-compatible database with CREATE TEMPORARY TABLES permission.
func TestSQLSession(t *testing.T) {
	addr := os.Getenv("SQLTR_TEST_MYSQL_ADDR")
	if addr == "" && *mariaBase == "" {
		t.Skip("set SQLTR_TEST_MYSQL_ADDR for the SQL integration test")
	}
	r := mapping{Address: addr, Database: os.Getenv("SQLTR_TEST_MYSQL_DB"), account: account{User: os.Getenv("SQLTR_TEST_MYSQL_USER")}, password: os.Getenv("SQLTR_TEST_MYSQL_PASSWORD")}
	if *mariaBase != "" {
		r = disposableDB(t)
	}
	for _, viaSSH := range []bool{false, true} {
		mode := "direct"
		if viaSSH {
			mode = "ssh"
		}
		t.Run(mode, func(t *testing.T) {
			g := newGateway()
			route := r
			if viaSSH {
				route = withTestJump(t, g, route, "password", false)
			}
			addr := startGateway(t, g, route)
			db := openDB(t, addr, "A", "front-secret")
			ctx := context.Background()
			c, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			var name string
			if err := c.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&name); err != nil || name != r.Database {
				t.Fatalf("db=%q err=%v", name, err)
			}
			if _, err = c.ExecContext(ctx, "CREATE TEMPORARY TABLE sqltr_session (v INT) ENGINE=InnoDB"); err != nil {
				t.Fatal(err)
			}
			tx, err := c.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec("INSERT INTO sqltr_session (v) VALUES (?)", 42); err != nil {
				t.Fatal(err)
			}
			var v int
			if err = tx.QueryRow("SELECT v FROM sqltr_session WHERE v = ?", 42).Scan(&v); err != nil || v != 42 {
				t.Fatalf("prepared read %d: %v", v, err)
			}
			_, err = tx.Exec("USE B")
			wantCode(t, err, mysql.ER_NOT_SUPPORTED_YET)
			if _, err = tx.Exec("USE A"); err != nil {
				t.Fatal(err)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err = c.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqltr_session").Scan(&v); err != nil || v != 0 {
				t.Fatalf("after rollback %d: %v", v, err)
			}
			wantCode(t, db.QueryRow("SELECT COUNT(*) FROM sqltr_session").Scan(&v), mysql.ER_NO_SUCH_TABLE)
		})
	}
}
