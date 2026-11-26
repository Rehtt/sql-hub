package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"github.com/pingcap/tidb/pkg/parser/charset"
	"golang.org/x/crypto/ssh"
)

const connectTimeout = 10 * time.Second

type backend struct {
	*client.Conn
	transport net.Conn // TCP connection to MySQL or the dedicated SSH channel
	release   func()
	closeOnce sync.Once
}

func (b *backend) close() {
	b.closeOnce.Do(func() {
		if b.transport != nil {
			_ = b.transport.Close()
		}
		if b.Conn != nil {
			_ = b.Conn.Close()
		}
		if b.release != nil {
			b.release()
		}
	})
}

func (g *gateway) acquireSSH(ctx context.Context, name string) (*ssh.Client, func(), error) {
	g.sshMu.Lock()
	defer g.sshMu.Unlock()
	if g.sshPool == nil {
		g.sshPool = make(map[string]*sharedSSH)
	}

	jump, ok := g.jumps[name]
	if !ok {
		return nil, nil, fmt.Errorf("unknown ssh host %q", name)
	}
	if shared := g.sshPool[name]; shared != nil {
		if shared.idleTimer != nil {
			shared.idleTimer.Stop()
			shared.idleTimer = nil
		}
		shared.refs++
		return shared.client, g.releaseSSH(name, shared), nil
	}

	var dialer net.Dialer
	transport, err := dialer.DialContext(ctx, "tcp", jump.address)
	if err != nil {
		return nil, nil, err
	}
	deadline, _ := ctx.Deadline()
	if err := transport.SetDeadline(deadline); err != nil {
		_ = transport.Close()
		return nil, nil, err
	}
	conn, chans, reqs, err := ssh.NewClientConn(transport, jump.address, jump.config)
	if err != nil {
		return nil, nil, err
	}
	client := ssh.NewClient(conn, chans, reqs)
	if err := transport.SetDeadline(time.Time{}); err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	shared := &sharedSSH{client: client, refs: 1}
	g.sshPool[name] = shared
	return client, g.releaseSSH(name, shared), nil
}

func (g *gateway) releaseSSH(name string, shared *sharedSSH) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.sshMu.Lock()
			defer g.sshMu.Unlock()
			if shared.refs == 0 {
				return
			}
			shared.refs--
			if shared.refs == 0 {
				timeout := g.sshIdleTimeout
				if timeout <= 0 {
					timeout = defaultSSHIdleTimeout
				}
				shared.idleTimer = time.AfterFunc(timeout, func() {
					g.expireSSH(name, shared)
				})
			}
		})
	}
}

func (g *gateway) expireSSH(name string, shared *sharedSSH) {
	g.sshMu.Lock()
	if g.sshPool[name] != shared || shared.refs != 0 {
		g.sshMu.Unlock()
		return
	}
	delete(g.sshPool, name)
	shared.idleTimer = nil
	g.sshMu.Unlock()
	_ = shared.client.Close()
}

func (g *gateway) connect(r mapping, front *server.Conn, settings sessionSettings) (*backend, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	var target net.Conn
	var release func()
	var err error
	if r.SSH != "" {
		client, releaseSSH, err := g.acquireSSH(ctx, r.SSH)
		if err != nil {
			return nil, err
		}
		release = releaseSSH
		target, err = client.DialContext(ctx, "tcp", r.Address)
		if err != nil {
			release()
			return nil, err
		}
	} else {
		var dialer net.Dialer
		target, err = dialer.DialContext(ctx, "tcp", r.Address)
		if err != nil {
			return nil, err
		}
	}
	connected := false
	var c *client.Conn
	defer func() {
		if !connected {
			if c != nil {
				_ = c.Close()
			}
			if target != nil {
				_ = target.Close()
			}
			if release != nil {
				release()
			}
		}
	}()
	deadline, _ := ctx.Deadline()
	if r.SSH == "" {
		if err := target.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}

	// Match packet formats on both legs so results and prepared statements can be streamed verbatim.
	option := func(c *client.Conn) error {
		c.UnsetCapability(mysql.CLIENT_QUERY_ATTRIBUTES | mysql.CLIENT_DEPRECATE_EOF)
		for _, flag := range []uint32{mysql.CLIENT_FOUND_ROWS, mysql.CLIENT_SESSION_TRACK, mysql.CLIENT_MULTI_RESULTS, mysql.CLIENT_PS_MULTI_RESULTS} {
			if front.HasCapability(flag) {
				if err := c.SetCapability(flag); err != nil {
					return err
				}
			}
		}
		col, err := charset.GetCollationByID(int(front.Charset()))
		if err != nil {
			return err
		}
		return c.SetCollation(col.Name)
	}
	useTarget := func(context.Context, string, string) (net.Conn, error) { return target, nil }
	c, err = client.ConnectWithDialer(ctx, "tcp", r.Address, r.User, r.password, r.Database, useTarget, option)
	if err != nil {
		return nil, fmt.Errorf("MySQL handshake: %w", err)
	}
	for _, query := range []string{settings.setNames, settings.setAutocommit} {
		if query == "" {
			continue
		}
		if _, err := c.Execute(query); err != nil {
			return nil, fmt.Errorf("initialize database session: %w", err)
		}
	}
	if r.SSH == "" {
		if err := target.SetDeadline(time.Time{}); err != nil {
			return nil, err
		}
	}
	connected = true
	return &backend{Conn: c, transport: target, release: release}, nil
}
