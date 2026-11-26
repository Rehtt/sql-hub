package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
	"go.yaml.in/yaml/v3"
)

func TestConfiguredGateway(t *testing.T) {
	r := targetFixture(t)
	data, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	delete(cfg, "proxy")
	cfg["gateway"].(map[string]any)["password"] = "front-secret"
	target := cfg["mappings"].(map[string]any)["A"].(map[string]any)
	target["address"] = r.Address
	target["password"] = r.password
	target["read_only"] = true
	for _, raw := range cfg["mappings"].(map[string]any) {
		delete(raw.(map[string]any), "proxy")
	}
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	writeConfig := func() {
		t.Helper()
		data, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig()
	g, err := loadGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	if !g.routes["A"].ReadOnly {
		t.Fatal("read_only mapping option was not loaded")
	}
	addr := listenTest(t, g.serve)
	wantCode(t, openDB(t, addr, "A", "wrong").Ping(), mysql.ER_ACCESS_DENIED_ERROR)
	db := openDB(t, addr, "A", "front-secret")
	var name string
	if err := db.QueryRow("SELECT DATABASE()").Scan(&name); err != nil || name != "AA" {
		t.Fatalf("database=%q err=%v", name, err)
	}
	target["proxy"] = "unknown"
	writeConfig()
	if _, err := loadGateway(path); err == nil {
		t.Fatal("accepted unknown jump host")
	}
}

func TestProxyConfigValidation(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name  string
		proxy proxyConfig
	}{
		{name: "unknown type", proxy: proxyConfig{Type: "http", Address: "127.0.0.1:8080"}},
		{name: "socks invalid address", proxy: proxyConfig{Type: proxySOCKS5, Address: "127.0.0.1"}},
		{name: "socks ssh field", proxy: proxyConfig{Type: proxySOCKS5, Address: "127.0.0.1:1080", KnownHosts: "known_hosts"}},
		{name: "socks incomplete auth", proxy: proxyConfig{Type: proxySOCKS5, Address: "127.0.0.1:1080", User: "proxy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.proxy.build(dir); err == nil {
				t.Fatal("accepted invalid proxy configuration")
			}
		})
	}
}
