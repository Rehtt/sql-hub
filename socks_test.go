package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"go.yaml.in/yaml/v3"
)

func socks5TestServer(t *testing.T, user, password string) string {
	t.Helper()
	requireAuth := user != ""
	return listenTest(t, func(raw net.Conn) {
		defer raw.Close()
		reader := bufio.NewReader(raw)
		header := make([]byte, 2)
		if _, err := io.ReadFull(reader, header); err != nil || header[0] != 5 {
			return
		}
		methods := make([]byte, int(header[1]))
		if _, err := io.ReadFull(reader, methods); err != nil {
			return
		}
		method := byte(0)
		if requireAuth {
			method = 2
			if !bytes.Contains(methods, []byte{method}) {
				_, _ = raw.Write([]byte{5, 0xff})
				return
			}
		} else if !bytes.Contains(methods, []byte{method}) {
			_, _ = raw.Write([]byte{5, 0xff})
			return
		}
		if _, err := raw.Write([]byte{5, method}); err != nil {
			return
		}
		if requireAuth {
			if !socks5TestAuth(reader, raw, user, password) {
				return
			}
		}

		request := make([]byte, 4)
		if _, err := io.ReadFull(reader, request); err != nil || request[0] != 5 || request[1] != 1 {
			return
		}
		host, port, ok := socks5TestAddress(reader, request[3])
		if !ok {
			return
		}
		target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			_, _ = raw.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
		defer target.Close()
		if _, err := raw.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			return
		}
		bridge(raw, target)
	})
}

func socks5TestAuth(reader *bufio.Reader, raw net.Conn, user, password string) bool {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil || header[0] != 1 {
		return false
	}
	name := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, name); err != nil {
		return false
	}
	if _, err := io.ReadFull(reader, header[:1]); err != nil {
		return false
	}
	pass := make([]byte, int(header[0]))
	if _, err := io.ReadFull(reader, pass); err != nil {
		return false
	}
	if string(name) != user || string(pass) != password {
		_, _ = raw.Write([]byte{1, 1})
		return false
	}
	_, err := raw.Write([]byte{1, 0})
	return err == nil
}

func socks5TestAddress(reader *bufio.Reader, atyp byte) (string, int, bool) {
	var host string
	switch atyp {
	case 1:
		address := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(reader, address); err != nil {
			return "", 0, false
		}
		host = net.IP(address).String()
	case 3:
		length, err := reader.ReadByte()
		if err != nil {
			return "", 0, false
		}
		address := make([]byte, int(length))
		if _, err := io.ReadFull(reader, address); err != nil {
			return "", 0, false
		}
		host = string(address)
	case 4:
		address := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(reader, address); err != nil {
			return "", 0, false
		}
		host = net.IP(address).String()
	default:
		return "", 0, false
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBytes); err != nil {
		return "", 0, false
	}
	return host, int(binary.BigEndian.Uint16(portBytes)), true
}

func TestSOCKS5Proxy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		user     string
		password string
	}{
		{name: "anonymous"},
		{name: "username and password", user: "proxy-user", password: "proxy-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := targetFixture(t)
			_, port, err := net.SplitHostPort(target.Address)
			if err != nil {
				t.Fatal(err)
			}
			target.Address = net.JoinHostPort("localhost", port)
			proxyAddress := socks5TestServer(t, tc.user, tc.password)
			proxy := map[string]any{
				"type":    proxySOCKS5,
				"address": proxyAddress,
			}
			if tc.user != "" {
				proxy["user"] = tc.user
				proxy["password"] = tc.password
			}
			cfg := map[string]any{
				"gateway": map[string]any{"user": "gateway", "password": "front-secret"},
				"proxy":   map[string]any{"test": proxy},
				"mappings": map[string]any{
					"A": map[string]any{
						"address":  target.Address,
						"database": target.Database,
						"user":     target.User,
						"password": target.password,
						"proxy":    "test",
					},
				},
			}
			data, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "gateway.yaml")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			gateway, err := loadGateway(path)
			if err != nil {
				t.Fatal(err)
			}
			addr := listenTest(t, gateway.serve)
			db := openDB(t, addr, "A", "front-secret")
			var name string
			if err := db.QueryRow("SELECT DATABASE()").Scan(&name); err != nil || name != target.Database {
				t.Fatalf("database=%q err=%v", name, err)
			}
		})
	}
}
