package main

import (
	"context"
	"fmt"
	"net"
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
	transport net.Conn // TCP connection to MySQL or the dedicated SSH transport
}

func (b *backend) close() {
	b.transport.Close()
	b.Conn.Close()
}

func (g *gateway) connect(r mapping, front *server.Conn, settings sessionSettings) (*backend, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	jump := g.jumps[r.SSH]
	address := r.Address
	if r.SSH != "" {
		address = jump.address
	}
	var dialer net.Dialer
	transport, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	connected := false
	defer func() {
		if !connected {
			transport.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	if err := transport.SetDeadline(deadline); err != nil {
		return nil, err
	}
	target := transport
	if r.SSH != "" {
		conn, chans, reqs, err := ssh.NewClientConn(transport, jump.address, jump.config)
		if err != nil {
			return nil, err
		}
		tunnel := ssh.NewClient(conn, chans, reqs)
		target, err = tunnel.DialContext(ctx, "tcp", r.Address)
		if err != nil {
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
	c, err := client.ConnectWithDialer(ctx, "tcp", r.Address, r.User, r.password, r.Database, useTarget, option)
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
	if err := transport.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	connected = true
	return &backend{c, transport}, nil
}
