package main

import (
	"bytes"
	"io"
	"log"
	"net"
	"regexp"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"github.com/pingcap/tidb/pkg/parser/charset"
)

type session struct {
	server.EmptyHandler
	*server.InMemoryAuthenticationHandler
	gateway   *gateway
	initialDB string
	settings  sessionSettings
	front     *server.Conn
	back      *backend
	alias     string
}

// The library reads the initial database before verifying the password.
func (s *session) UseDB(name string) error {
	s.initialDB = name
	return nil
}

func (s *session) OnAuthSuccess(c *server.Conn) error {
	s.front = c
	c.SetStatus(mysql.SERVER_STATUS_AUTOCOMMIT)
	if s.initialDB != "" {
		return s.bind(s.initialDB)
	}
	return nil
}

func (s *session) bind(alias string) error {
	r, ok := s.gateway.routes[alias]
	if !ok {
		return mysql.NewDefaultError(mysql.ER_BAD_DB_ERROR, alias)
	}
	if s.back != nil {
		if s.alias != alias {
			return mysql.NewError(mysql.ER_NOT_SUPPORTED_YET, "Reconnect to select another database mapping")
		}
		return nil
	}
	b, err := s.gateway.connect(r, s.front, s.settings)
	if err != nil {
		log.Printf("connecting mapping %q: %v", alias, err)
		return mysql.NewError(mysql.ER_CONNECT_TO_FOREIGN_DATA_SOURCE, "Could not connect to the mapped database")
	}
	s.alias, s.back = alias, b
	return nil
}

func (g *gateway) serve(raw net.Conn) {
	defer raw.Close()
	// Isolate malformed input to this client's connection, including library decoder panics.
	defer func() {
		if err := recover(); err != nil {
			log.Printf("MySQL session aborted: %v", err)
		}
	}()
	s := &session{gateway: g, InMemoryAuthenticationHandler: g.auth}
	defer func() {
		if s.back != nil {
			s.back.close()
		}
	}()
	raw.SetDeadline(time.Now().Add(connectTimeout * 2))
	if _, err := g.server.NewCustomizedConn(raw, s, s); err != nil {
		return
	}
	raw.SetDeadline(time.Time{})

	var relaying bool
	for {
		if s.back != nil && !relaying {
			// The backend has completed its handshake and cannot send result bytes
			// until the first command. Closing the front also interrupts idle readers.
			upstream := s.back.Conn.Conn.Conn
			go func() { _, _ = io.Copy(raw, upstream); raw.Close() }()
			relaying = true
		}
		s.front.ResetSequence()
		data, err := s.front.ReadPacket()
		if err != nil || len(data) == 0 || data[0] == mysql.COM_QUIT {
			return
		}
		if err := s.command(data); err != nil {
			return
		}
	}
}

func (s *session) command(data []byte) error {
	cmd, payload := data[0], data[1:]
	var name string
	selecting := cmd == mysql.COM_INIT_DB
	if selecting {
		name = string(payload)
	}
	if cmd == mysql.COM_QUERY || cmd == mysql.COM_STMT_PREPARE {
		var err error
		name, selecting, err = useDatabase(payload)
		if err != nil {
			return s.front.WriteValue(err)
		}
		if selecting && cmd == mysql.COM_STMT_PREPARE {
			return s.front.WriteValue(mysql.NewDefaultError(mysql.ER_UNSUPPORTED_PS))
		}
	}
	if selecting {
		wasBound := s.back != nil
		if err := s.bind(name); err != nil {
			return s.front.WriteValue(err)
		}
		if !wasBound {
			return s.front.WriteValue(nil)
		}
		// Ask the existing session to reselect the real database. Its reply
		// carries the current transaction flags and session-tracking data.
		data = append([]byte{mysql.COM_INIT_DB}, s.gateway.routes[name].Database...)
	}
	if s.back == nil {
		if cmd == mysql.COM_PING {
			return s.front.WriteValue(nil)
		}
		if cmd == mysql.COM_QUERY {
			return s.queryBeforeSelection(trimSQL(payload))
		}
		return s.front.WriteValue(mysql.NewDefaultError(mysql.ER_NO_DB_ERROR))
	}
	switch data[0] {
	case mysql.COM_QUERY, mysql.COM_INIT_DB, mysql.COM_FIELD_LIST, mysql.COM_PING,
		mysql.COM_STMT_PREPARE, mysql.COM_STMT_EXECUTE, mysql.COM_STMT_SEND_LONG_DATA,
		mysql.COM_STMT_CLOSE, mysql.COM_STMT_RESET, mysql.COM_STMT_FETCH, mysql.COM_STATISTICS:
		s.back.ResetSequence()
		packet := make([]byte, 4, len(data)+4)
		return s.back.WritePacket(append(packet, data...))
	default:
		return s.front.WriteValue(mysql.NewDefaultError(mysql.ER_UNKNOWN_COM_ERROR))
	}
}

// PyMySQL configures the session before the user can choose a database.
type sessionSettings struct {
	setNames      string
	setAutocommit string
}

var setNamesQuery = regexp.MustCompile(`(?i)^SET\s+NAMES\s+([a-z0-9_]+)(?:\s+COLLATE\s+([a-z0-9_]+))?\s*;?$`)
var setAutocommitQuery = regexp.MustCompile(`(?i)^SET\s+AUTOCOMMIT\s*=\s*([01])\s*;?$`)

func (s *session) configureBeforeSelection(query []byte) error {
	if m := setNamesQuery.FindSubmatch(query); m != nil {
		if !charset.ValidCharsetAndCollation(string(m[1]), string(m[2])) {
			return mysql.NewError(mysql.ER_WRONG_VALUE_FOR_VAR, "Invalid character set or collation for SET NAMES")
		}
		s.settings.setNames = "SET NAMES " + string(m[1])
		if len(m[2]) != 0 {
			s.settings.setNames += " COLLATE " + string(m[2])
		}
		return nil
	}
	if m := setAutocommitQuery.FindSubmatch(query); m != nil {
		s.settings.setAutocommit = "SET AUTOCOMMIT = " + string(m[1])
		if m[1][0] == '1' {
			s.front.SetStatus(mysql.SERVER_STATUS_AUTOCOMMIT)
		} else {
			s.front.UnsetStatus(mysql.SERVER_STATUS_AUTOCOMMIT)
		}
		return nil
	}
	return mysql.NewDefaultError(mysql.ER_NO_DB_ERROR)
}

// Skip ordinary MySQL comments anywhere trivia is legal in a USE statement.
func trimSQL(b []byte) []byte {
	for {
		b = bytes.TrimSpace(b)
		switch {
		case bytes.HasPrefix(b, []byte("/*")) && !bytes.HasPrefix(b, []byte("/*!")) && !bytes.HasPrefix(b, []byte("/*M!")):
			i := bytes.Index(b[2:], []byte("*/"))
			if i < 0 {
				return b
			}
			b = b[i+4:]
		case bytes.HasPrefix(b, []byte("#")) || (bytes.HasPrefix(b, []byte("--")) && len(b) > 2 && b[2] <= ' '):
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				return nil
			}
			b = b[i+1:]
		default:
			return b
		}
	}
}

func useDatabase(query []byte) (string, bool, error) {
	q := trimSQL(query)
	// Expand only leading executable comments for command classification. Their
	// version predicates belong to the target, so never route a conditional USE.
	probe := q
	conditional := false
	for bytes.HasPrefix(probe, []byte("/*!")) || bytes.HasPrefix(probe, []byte("/*M!")) {
		end := bytes.Index(probe, []byte("*/"))
		if end < 0 {
			return "", false, nil
		}
		start := bytes.IndexByte(probe, '!') + 1
		body := bytes.TrimLeft(probe[start:end], "0123456789")
		if len(trimSQL(body)) != 0 {
			probe = trimSQL(body)
			conditional = true
			break
		}
		probe = trimSQL(probe[end+2:])
	}
	if len(probe) < 3 || !bytes.EqualFold(probe[:3], []byte("USE")) || (len(probe) > 3 && identifierByte(probe[3])) {
		return "", false, nil
	}
	if conditional {
		return "", true, mysql.NewError(mysql.ER_NOT_SUPPORTED_YET, "Use an explicit USE statement to select a mapping")
	}
	q = probe
	q = trimSQL(q[3:])
	syntax := mysql.NewError(mysql.ER_PARSE_ERROR, "Expected USE followed by one database alias")
	var name []byte
	if len(q) > 0 && q[0] == 96 {
		q = q[1:]
		for {
			i := bytes.IndexByte(q, 96)
			if i < 0 {
				return "", true, syntax
			}
			name = append(name, q[:i]...)
			q = q[i+1:]
			if len(q) == 0 || q[0] != 96 {
				break
			}
			name = append(name, 96)
			q = q[1:]
		}
	} else {
		i := 0
		for i < len(q) && identifierByte(q[i]) {
			i++
		}
		name, q = q[:i], q[i:]
	}
	q = trimSQL(q)
	if len(q) > 0 && q[0] == ';' {
		q = trimSQL(q[1:])
	}
	if len(name) == 0 || len(q) > 0 {
		return "", true, syntax
	}
	return string(name), true, nil
}

func identifierByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$' || c >= 0x80
}
