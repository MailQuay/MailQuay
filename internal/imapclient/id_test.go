package imapclient

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptServer is a minimal IMAP server that advertises a fixed capability
// list, answers every command with OK and records the raw command lines.
type scriptServer struct {
	ln       net.Listener
	caps     []string
	rejectID bool

	mu   sync.Mutex
	cmds []string
	conn net.Conn

	done chan struct{}
}

func startScriptServer(t *testing.T, rejectID bool, caps ...string) *scriptServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &scriptServer{ln: ln, caps: caps, rejectID: rejectID, done: make(chan struct{})}
	go s.serve()
	t.Cleanup(s.close)
	return s
}

func (s *scriptServer) addr() string { return s.ln.Addr().String() }

func (s *scriptServer) close() {
	_ = s.ln.Close()
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.mu.Unlock()
	<-s.done
}

func (s *scriptServer) serve() {
	defer close(s.done)
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	defer conn.Close()

	if _, err := fmt.Fprint(conn, "* OK script server ready\r\n"); err != nil {
		return
	}
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		s.mu.Lock()
		s.cmds = append(s.cmds, line)
		s.mu.Unlock()

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tag, verb := fields[0], strings.ToUpper(fields[1])
		switch verb {
		case "CAPABILITY":
			_, _ = fmt.Fprintf(conn, "* CAPABILITY %s\r\n%s OK CAPABILITY completed\r\n", strings.Join(s.caps, " "), tag)
		case "ID":
			if s.rejectID {
				_, _ = fmt.Fprintf(conn, "%s BAD ID rejected\r\n", tag)
			} else {
				_, _ = fmt.Fprintf(conn, "* ID NIL\r\n%s OK ID completed\r\n", tag)
			}
		case "LOGOUT":
			_, _ = fmt.Fprintf(conn, "* BYE closing\r\n%s OK LOGOUT completed\r\n", tag)
		default:
			_, _ = fmt.Fprintf(conn, "%s OK %s completed\r\n", tag, verb)
		}
	}
}

func (s *scriptServer) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

func dialScriptServer(t *testing.T, srv *scriptServer) *Client {
	t.Helper()
	c, err := Dial(context.Background(), Config{
		Address: srv.addr(), Security: SecurityNone,
		Username: "alice", Password: "secret", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func commandIndex(cmds []string, verb string) int {
	for i, line := range cmds {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[1], verb) {
			return i
		}
	}
	return -1
}

func countCommand(cmds []string, verb string) int {
	count := 0
	for _, line := range cmds {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[1], verb) {
			count++
		}
	}
	return count
}

func TestDialSendsIDAfterLogin(t *testing.T) {
	SetVersion("9.9.9")
	t.Cleanup(func() { SetVersion("dev") })

	srv := startScriptServer(t, false, "IMAP4rev1", "ID")
	dialScriptServer(t, srv)

	cmds := srv.commands()
	login, id := commandIndex(cmds, "LOGIN"), commandIndex(cmds, "ID")
	if login < 0 || id < 0 {
		t.Fatalf("commands = %q, want LOGIN and ID", cmds)
	}
	if id < login {
		t.Fatalf("commands = %q, want ID after LOGIN", cmds)
	}
	if count := countCommand(cmds, "ID"); count != 1 {
		t.Fatalf("ID sent %d times, want once: %q", count, cmds)
	}
	if want := `ID ("name" "MailQuay" "version" "9.9.9")`; !strings.HasSuffix(cmds[id], want) {
		t.Fatalf("ID command = %q, want suffix %q", cmds[id], want)
	}
}

func TestDialSkipsIDWhenNotAdvertised(t *testing.T) {
	srv := startScriptServer(t, false, "IMAP4rev1")
	dialScriptServer(t, srv)

	if id := commandIndex(srv.commands(), "ID"); id >= 0 {
		t.Fatalf("ID sent to a server without the capability: %q", srv.commands()[id])
	}
}

func TestDialToleratesRejectedID(t *testing.T) {
	srv := startScriptServer(t, true, "IMAP4rev1", "ID")

	client := dialScriptServer(t, srv)
	if client.Closed() {
		t.Fatal("connection closed after the server rejected ID")
	}
	if id := commandIndex(srv.commands(), "ID"); id < 0 {
		t.Fatal("ID command was not attempted")
	}
}
