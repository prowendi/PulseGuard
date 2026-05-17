package smtp

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- unit tests for the pure helpers -------------------------------

func TestParseToken_Happy(t *testing.T) {
	tok := buildToken("alice@example.com", "s3cr3t!@/=", "smtp.example.com", 587, "Alerts <alerts@example.com>", true)
	cfg, err := parseToken(tok)
	if err != nil {
		t.Fatalf("parseToken: %v", err)
	}
	if cfg.host != "smtp.example.com" {
		t.Errorf("host = %q", cfg.host)
	}
	if cfg.port != 587 {
		t.Errorf("port = %d", cfg.port)
	}
	if cfg.username != "alice@example.com" {
		t.Errorf("user = %q", cfg.username)
	}
	if cfg.password != "s3cr3t!@/=" {
		t.Errorf("pass = %q (URL-encoding round-trip broken)", cfg.password)
	}
	if cfg.from != "Alerts <alerts@example.com>" {
		t.Errorf("from = %q", cfg.from)
	}
	if !cfg.useTLS {
		t.Errorf("useTLS = false, want true")
	}
}

func TestParseToken_Malformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"telegram://nope",
		"smtp://",
		"smtp://host/no-userinfo",
		"smtp://just-user@host", // no password is fine; we accept empty
	} {
		_, err := parseToken(bad)
		if err == nil {
			// "no password" specifically is allowed — it's a relay
			// without auth. Filter out of the negative-case loop.
			if bad == "smtp://just-user@host" {
				continue
			}
			t.Errorf("parseToken(%q) returned nil err, want failure", bad)
		}
	}
}

func TestParseRecipients(t *testing.T) {
	cases := map[string][]string{
		"alice@example.com":                                       {"alice@example.com"},
		"  alice@example.com  ":                                   {"alice@example.com"},
		"alice@example.com,bob@example.com":                       {"alice@example.com", "bob@example.com"},
		"alice@example.com, bob@example.com , carol@example.com,": {"alice@example.com", "bob@example.com", "carol@example.com"},
	}
	for in, want := range cases {
		got, err := parseRecipients(in)
		if err != nil {
			t.Errorf("parseRecipients(%q): %v", in, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("parseRecipients(%q) len = %d want %d", in, len(got), len(want))
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("parseRecipients(%q)[%d] = %q want %q", in, i, got[i], want[i])
			}
		}
	}
	// Malformed addresses must fail rather than silently shipping a
	// half-list — a typo in chat_id shouldn't quietly drop one of the
	// intended recipients.
	for _, bad := range []string{"", "   ", "no-at-sign", "@no-local", "no-domain@"} {
		if _, err := parseRecipients(bad); err == nil {
			t.Errorf("parseRecipients(%q) accepted invalid input", bad)
		}
	}
}

func TestSplitSubjectBody(t *testing.T) {
	cases := []struct {
		in           string
		wantSubject  string
		wantBodyHas  string // substring assertion (newline handling)
	}{
		// Standard "headline\n\ndetail" pattern → first line subject,
		// rest body.
		{"CPU spike on db01\n\n95% sustained for 5min", "CPU spike on db01", "95% sustained"},
		// Single-line body — entire text is subject, body empty.
		{"Disk full on /var", "Disk full on /var", ""},
		// Leading blank lines stripped.
		{"\n\n\nReal subject\n\nReal body", "Real subject", "Real body"},
		// Multi-line subject without blank-line separator → first line
		// only.
		{"Line one\nLine two\nLine three", "Line one", "Line two"},
		// Empty input → fallback subject.
		{"", "[PulseGuard alert]", ""},
	}
	for _, c := range cases {
		gotSubj, gotBody := splitSubjectBody(c.in)
		if gotSubj != c.wantSubject {
			t.Errorf("splitSubjectBody(%q) subject = %q want %q", c.in, gotSubj, c.wantSubject)
		}
		if c.wantBodyHas != "" && !strings.Contains(gotBody, c.wantBodyHas) {
			t.Errorf("splitSubjectBody(%q) body = %q, want substring %q", c.in, gotBody, c.wantBodyHas)
		}
		if c.wantBodyHas == "" && strings.TrimSpace(gotBody) != "" {
			t.Errorf("splitSubjectBody(%q) body = %q, want empty", c.in, gotBody)
		}
	}
}

func TestBuildMessage_HTMLAndPlain(t *testing.T) {
	plain := buildMessage("a@x", []string{"b@x", "c@x"}, "Hi", "world", "None")
	if !strings.Contains(string(plain), "Content-Type: text/plain") {
		t.Errorf("plain MIME missing")
	}
	if !strings.Contains(string(plain), "To: b@x, c@x") {
		t.Errorf("To header missing recipients: %s", plain)
	}
	if !strings.Contains(string(plain), "Subject: Hi") {
		t.Errorf("Subject missing")
	}

	html := buildMessage("a@x", []string{"b@x"}, "Hi", "<b>world</b>", "HTML")
	if !strings.Contains(string(html), "Content-Type: text/html") {
		t.Errorf("html MIME missing")
	}
}

func TestBuildMessage_NonASCIISubjectGetsRFC2047(t *testing.T) {
	msg := buildMessage("a@x", []string{"b@x"}, "告警: db01", "body", "None")
	// =?UTF-8?B?...?= MIME-word encoding for Chinese characters
	if !strings.Contains(string(msg), "=?UTF-8?B?") {
		t.Errorf("non-ASCII subject not MIME-encoded: %s", msg)
	}
	if !strings.Contains(string(msg), "?=") {
		t.Errorf("MIME-word terminator missing")
	}
}

func TestBuildMessage_DotStuffing(t *testing.T) {
	// A line that starts with a single "." would otherwise terminate
	// the DATA stream early. Verify the encoder doubles it.
	msg := buildMessage("a@x", []string{"b@x"}, "Hi", ".This line starts with a dot", "None")
	if !strings.Contains(string(msg), "..This line starts with a dot") {
		t.Errorf("dot-stuffing missing in: %s", msg)
	}
}

// --- integration test against a fake SMTP listener -----------------

// fakeSMTPServer accepts a single TCP connection and walks through a
// minimal SMTP transaction (EHLO → AUTH PLAIN → MAIL FROM → RCPT TO →
// DATA → "."). Captures the DATA section for assertion.
type fakeSMTPServer struct {
	listener net.Listener
	wg       sync.WaitGroup

	mu       sync.Mutex
	dataSeen []byte
	rcpts    []string
	mailFrom string
	authLine string
}

func newFakeSMTP(t *testing.T) *fakeSMTPServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fakeSMTPServer{listener: l}
	srv.wg.Add(1)
	go srv.serve()
	t.Cleanup(func() {
		_ = l.Close()
		srv.wg.Wait()
	})
	return srv
}

func (s *fakeSMTPServer) addr() string { return s.listener.Addr().String() }

func (s *fakeSMTPServer) serve() {
	defer s.wg.Done()
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	write := func(line string) {
		_, _ = bw.WriteString(line + "\r\n")
		_ = bw.Flush()
	}
	write("220 fake-smtp ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			// Advertise AUTH PLAIN so net/smtp's client uses it.
			write("250-fake-smtp")
			write("250-AUTH PLAIN")
			write("250 PIPELINING")
		case strings.HasPrefix(cmd, "AUTH PLAIN"):
			s.mu.Lock()
			s.authLine = cmd
			s.mu.Unlock()
			write("235 OK")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			s.mu.Lock()
			s.mailFrom = cmd
			s.mu.Unlock()
			write("250 OK")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			s.mu.Lock()
			s.rcpts = append(s.rcpts, cmd)
			s.mu.Unlock()
			write("250 OK")
		case cmd == "DATA":
			write("354 send data")
			var buf strings.Builder
			for {
				ln, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if ln == ".\r\n" || ln == ".\n" {
					break
				}
				buf.WriteString(ln)
			}
			s.mu.Lock()
			s.dataSeen = []byte(buf.String())
			s.mu.Unlock()
			write("250 message accepted")
		case cmd == "QUIT":
			write("221 bye")
			return
		case cmd == "RSET":
			write("250 OK")
		default:
			write("502 unrecognised: " + cmd)
		}
	}
}

func TestClient_Send_EndToEnd_Plaintext(t *testing.T) {
	srv := newFakeSMTP(t)
	// Build token with tls=0 so the client skips STARTTLS — our fake
	// server doesn't speak TLS.
	host, port := splitHostPort(t, srv.addr())
	tok := buildToken("alice@example.com", "wonderland", host, port, "", false)

	c := New(2 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.Send(ctx, tok, "bob@example.com,carol@example.com",
		"None",
		"CPU spike on db01\n\n95% sustained for 5min"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	data := string(srv.dataSeen)
	if !strings.Contains(data, "Subject: CPU spike on db01") {
		t.Errorf("Subject not derived from first line: data=%s", data)
	}
	if !strings.Contains(data, "95% sustained for 5min") {
		t.Errorf("body missing: data=%s", data)
	}
	if !strings.Contains(data, "To: bob@example.com, carol@example.com") {
		t.Errorf("multi-recipient To header malformed: data=%s", data)
	}
	if len(srv.rcpts) != 2 {
		t.Errorf("RCPT TO count = %d, want 2", len(srv.rcpts))
	}
}

func TestClient_Send_RejectsInvalidRecipient(t *testing.T) {
	srv := newFakeSMTP(t)
	host, port := splitHostPort(t, srv.addr())
	tok := buildToken("u@x", "p", host, port, "", false)
	c := New(2 * time.Second)
	_, err := c.Send(context.Background(), tok, "garbage-no-at-sign", "None", "ok")
	if err == nil {
		t.Fatalf("invalid recipient should fail")
	}
	ae, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if ae.Class != PermanentClient {
		t.Fatalf("class = %v, want PermanentClient", ae.Class)
	}
}

func TestClient_Send_DialFailure_Transient(t *testing.T) {
	// Closed port — connect immediately fails. Must classify as
	// Transient so the worker retries (network blips, relay restart).
	c := New(500 * time.Millisecond)
	tok := buildToken("u@x", "p", "127.0.0.1", 1, "", false) // port 1 — refused
	_, err := c.Send(context.Background(), tok, "x@y", "None", "subj\n\nbody")
	if err == nil {
		t.Fatalf("connect to port 1 should fail")
	}
	ae, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if !ae.IsTransient() {
		t.Fatalf("class = %v, want Transient", ae.Class)
	}
}

func TestClient_Send_BadToken(t *testing.T) {
	c := New(time.Second)
	_, err := c.Send(context.Background(), "not-a-smtp-url", "x@y", "None", "z")
	if !errors.Is(err, ErrBadBotToken) {
		t.Fatalf("err = %v, want ErrBadBotToken", err)
	}
}

func TestClassString(t *testing.T) {
	if Transient.String() != "transient" {
		t.Fatalf("Transient string = %s", Transient)
	}
	if PermanentClient.String() != "permanent_client" {
		t.Fatalf("PermanentClient string = %s", PermanentClient)
	}
	if Class(99).String() != "unknown" {
		t.Fatalf("unknown bucket missing")
	}
}

func TestAPIErrorNil(t *testing.T) {
	var e *APIError
	if got := e.Error(); got != "<nil smtp.APIError>" {
		t.Fatalf("nil APIError = %s", got)
	}
}

// --- helpers --------------------------------------------------------

func buildToken(user, pass, host string, port int, from string, useTLS bool) string {
	q := url.Values{}
	if from != "" {
		q.Set("from", from)
	}
	if useTLS {
		q.Set("tls", "1")
	} else {
		q.Set("tls", "0")
	}
	u := url.URL{
		Scheme:   "smtp",
		User:     url.UserPassword(user, pass),
		Host:     fmt.Sprintf("%s:%d", host, port),
		RawQuery: q.Encode(),
	}
	return u.String()
}

func splitHostPort(t *testing.T, hp string) (string, int) {
	t.Helper()
	host, ps, err := net.SplitHostPort(hp)
	if err != nil {
		t.Fatalf("split addr %q: %v", hp, err)
	}
	var p int
	_, err = fmt.Sscanf(ps, "%d", &p)
	if err != nil {
		t.Fatalf("parse port %q: %v", ps, err)
	}
	return host, p
}

// Keep these imports active even if a future refactor drops some
// reach into tls/io — both are convenient to have on the standby
// list for fake-server iteration.
var (
	_ = tls.Config{}
	_ = io.EOF
)
