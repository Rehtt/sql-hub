package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func bridge(a io.ReadWriteCloser, b io.ReadWriteCloser) {
	defer a.Close()
	defer b.Close()
	go func() { io.Copy(a, b); a.Close() }()
	io.Copy(b, a)
}

func withTestJump(t *testing.T, g *gateway, r mapping, method string, mismatch bool) mapping {
	t.Helper()
	dir := t.TempDir()
	key := func() (ed25519.PrivateKey, ssh.Signer) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		s, err := ssh.NewSignerFromKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		return priv, s
	}
	_, host := key()
	priv, user := key()
	serverConf := &ssh.ServerConfig{}
	j := jumpConfig{account: account{User: "jump"}, KnownHosts: "known_hosts"}
	if method == "password" || method == "password_env" {
		if method == "password_env" {
			t.Setenv("SQLTR_TEST_SSH_SECRET", "jump-secret")
			j.PasswordEnv = "SQLTR_TEST_SSH_SECRET"
		} else {
			j.Password = "jump-secret"
		}
		serverConf.PasswordCallback = func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if c.User() == "jump" && string(p) == "jump-secret" {
				return nil, nil
			}
			return nil, errors.New("bad login")
		}
	} else {
		block, err := ssh.MarshalPrivateKey(priv, "test")
		if err != nil {
			t.Fatal(err)
		}
		j.PrivateKey = "id"
		if err = os.WriteFile(filepath.Join(dir, "id"), pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
		serverConf.PublicKeyCallback = func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == "jump" && bytes.Equal(k.Marshal(), user.PublicKey().Marshal()) {
				return nil, nil
			}
			return nil, errors.New("bad key")
		}
	}
	serverConf.AddHostKey(host)
	realTarget := r.Address
	// Only the jump host knows how to reach this name.
	r.Address = "mysql.internal:3306"
	j.Address = listenTest(t, func(raw net.Conn) {
		defer raw.Close()
		conn, chans, reqs, err := ssh.NewServerConn(raw, serverConf)
		if err != nil {
			return
		}
		defer conn.Close()
		go ssh.DiscardRequests(reqs)
		for ch := range chans {
			var p struct {
				Host       string
				Port       uint32
				Origin     string
				OriginPort uint32
			}
			if ch.ChannelType() != "direct-tcpip" || ssh.Unmarshal(ch.ExtraData(), &p) != nil || net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))) != "mysql.internal:3306" {
				ch.Reject(ssh.Prohibited, "unexpected destination")
				continue
			}
			target, err := net.Dial("tcp", realTarget)
			if err != nil {
				ch.Reject(ssh.ConnectionFailed, "target unavailable")
				continue
			}
			channel, requests, err := ch.Accept()
			if err != nil {
				target.Close()
				continue
			}
			go ssh.DiscardRequests(requests)
			go bridge(channel, target)
		}
	})
	trusted := host
	if mismatch {
		_, trusted = key()
	}
	line := knownhosts.Line([]string{j.Address}, trusted.PublicKey())
	if err := os.WriteFile(filepath.Join(dir, j.KnownHosts), []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	jump, err := j.build(dir)
	if err != nil {
		t.Fatal(err)
	}
	g.proxies["test"] = proxyTarget{kind: proxySSH, jump: jump}
	r.Proxy = "test"
	return r
}

func TestJumpHost(t *testing.T) {
	for _, method := range []string{"password", "password_env", "key"} {
		for _, mismatch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mismatch=%v", method, mismatch), func(t *testing.T) {
				g := newGateway()
				r := withTestJump(t, g, targetFixture(t), method, mismatch)
				db := openDB(t, startGateway(t, g, r), "A", "front-secret")
				var name string
				err := db.QueryRow("SELECT DATABASE()").Scan(&name)
				if mismatch {
					wantCode(t, err, mysql.ER_CONNECT_TO_FOREIGN_DATA_SOURCE)
					return
				}
				if err != nil || name != "AA" {
					t.Fatalf("database=%q err=%v", name, err)
				}
			})
		}
	}
}
