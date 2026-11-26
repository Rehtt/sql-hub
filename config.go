package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type account struct {
	User        string `yaml:"user"`
	Password    string `yaml:"password,omitempty"`
	PasswordEnv string `yaml:"password_env"`
}

type jumpConfig struct {
	account    `yaml:",inline"`
	Address    string `yaml:"address"`
	PrivateKey string `yaml:"private_key"`
	KnownHosts string `yaml:"known_hosts"`
}

type mapping struct {
	account  `yaml:",inline"`
	Address  string `yaml:"address"`
	Database string `yaml:"database"`
	SSH      string `yaml:"ssh"`
	ReadOnly bool   `yaml:"read_only,omitempty"`
	password string
}

type jumpHost struct {
	address string
	config  *ssh.ClientConfig
}

type sharedSSH struct {
	client    *ssh.Client
	refs      int
	idleTimer *time.Timer
}

const defaultSSHIdleTimeout = 5 * time.Minute

type gateway struct {
	server         *server.Server
	auth           *server.InMemoryAuthenticationHandler
	routes         map[string]mapping
	jumps          map[string]jumpHost
	sshMu          sync.Mutex
	sshPool        map[string]*sharedSSH
	sshIdleTimeout time.Duration
}

func newGateway() *gateway {
	return &gateway{
		server:         server.NewServer("8.0.11-sqlG", mysql.DEFAULT_COLLATION_ID, mysql.AUTH_NATIVE_PASSWORD, nil, nil),
		auth:           server.NewInMemoryAuthenticationHandler(mysql.AUTH_NATIVE_PASSWORD),
		jumps:          make(map[string]jumpHost),
		sshPool:        make(map[string]*sharedSSH),
		sshIdleTimeout: defaultSSHIdleTimeout,
	}
}

func (a account) passwordValue() (string, error) {
	if a.User == "" {
		return "", errors.New("user is required")
	}
	if (a.Password == "") == (a.PasswordEnv == "") {
		return "", errors.New("set exactly one of password or password_env")
	}
	if a.Password != "" {
		return a.Password, nil
	}
	pw := os.Getenv(a.PasswordEnv)
	if pw == "" {
		return "", fmt.Errorf("environment variable %q is empty or missing", a.PasswordEnv)
	}
	return pw, nil
}

func validAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	n, errPort := strconv.Atoi(port)
	return err == nil && host != "" && errPort == nil && n > 0 && n <= 65535
}

func (j jumpConfig) build(dir string) (jumpHost, error) {
	if !validAddress(j.Address) || j.User == "" || j.KnownHosts == "" {
		return jumpHost{}, errors.New("expected address, user and known_hosts")
	}
	if j.PrivateKey != "" && (j.Password != "" || j.PasswordEnv != "") {
		return jumpHost{}, errors.New("private_key cannot be combined with password or password_env")
	}
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(dir, path)
	}
	check, err := knownhosts.New(resolve(j.KnownHosts))
	if err != nil {
		return jumpHost{}, err
	}
	var auth ssh.AuthMethod
	if j.PrivateKey != "" {
		key, err := os.ReadFile(resolve(j.PrivateKey))
		if err != nil {
			return jumpHost{}, err
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return jumpHost{}, err
		}
		auth = ssh.PublicKeys(signer)
	} else {
		pw, err := j.passwordValue()
		if err != nil {
			return jumpHost{}, err
		}
		auth = ssh.Password(pw)
	}
	return jumpHost{j.Address, &ssh.ClientConfig{User: j.User, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: check}}, nil
}

func loadGateway(path string) (*gateway, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cfg struct {
		Gateway        account               `yaml:"gateway"`
		SSH            map[string]jumpConfig `yaml:"ssh"`
		Mappings       map[string]mapping    `yaml:"mappings"`
		SSHIdleTimeout *string               `yaml:"ssh_idle_timeout"`
	}
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(yaml.Node)); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("expected a single YAML configuration document")
	}
	if len(cfg.Mappings) == 0 {
		return nil, errors.New("at least one database mapping is required")
	}
	sshIdleTimeout := defaultSSHIdleTimeout
	if cfg.SSHIdleTimeout != nil {
		sshIdleTimeout, err = time.ParseDuration(*cfg.SSHIdleTimeout)
		if err != nil {
			return nil, fmt.Errorf("ssh_idle_timeout: %w", err)
		}
		if sshIdleTimeout <= 0 {
			return nil, errors.New("ssh_idle_timeout must be positive")
		}
	}

	g := newGateway()
	g.sshIdleTimeout = sshIdleTimeout
	password, err := cfg.Gateway.passwordValue()
	if err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}
	if err := g.auth.AddUser(cfg.Gateway.User, password); err != nil {
		return nil, err
	}
	for name, j := range cfg.SSH {
		host, err := j.build(filepath.Dir(path))
		if err != nil {
			return nil, fmt.Errorf("ssh %q: %w", name, err)
		}
		g.jumps[name] = host
	}
	g.routes = cfg.Mappings
	for alias, r := range g.routes {
		if alias == "" || r.Database == "" || !validAddress(r.Address) {
			return nil, fmt.Errorf("mapping %q needs an alias, target database and host:port", alias)
		}
		if _, ok := g.jumps[r.SSH]; r.SSH != "" && !ok {
			return nil, fmt.Errorf("mapping %q: unknown ssh host %q", alias, r.SSH)
		}
		r.password, err = r.passwordValue()
		if err != nil {
			return nil, fmt.Errorf("mapping %q: %w", alias, err)
		}
		g.routes[alias] = r
	}
	return g, nil
}
