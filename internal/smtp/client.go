// Package smtp implements the outbound mail Sender for PulseGuard's
// SMTP delivery platform. It exposes a Client that satisfies
// domain.Sender so the worker pool can deliver alerts via email
// using the same contract as Telegram / Lark.
//
// Wire shape (consumed by the worker through sender_router):
//
//	botToken = "smtp://<user>:<pass>@<host>:<port>?from=<from>&tls=<0|1>"
//	chatID   = "alice@example.com[,bob@example.com[,...]]"  (comma-separated To: list)
//	parseMode = "HTML" → Content-Type: text/html, anything else → text/plain
//	text     = template-rendered body. The FIRST non-empty line becomes
//	           the Subject:, the rest (after the first "\n\n", if any)
//	           becomes the message body.
//
// Authentication is PLAIN over either STARTTLS (port 587 by convention)
// or implicit TLS (port 465 by convention). Plaintext (tls=0) is
// supported only for localhost testing — production deployments MUST
// set use_tls=true.
//
// Error classification mirrors lark.APIError / tg.APIError:
//
//   - Transient: dial failure, TLS handshake error, network timeout,
//     server 4xx temporary (e.g. 421/450/451/452), 5xx replies.
//   - PermanentClient: bad credentials (535), bad recipient address
//     syntax, unknown 5xx codes that explicitly indicate config (550).
package smtp

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout caps how long a single Send takes end-to-end (dial +
// TLS + auth + DATA). Telegram / Lark are typically <1s; SMTP relays
// commonly take 2-3s, so a generous default leaves headroom without
// letting a hung connection pin a worker forever.
const DefaultTimeout = 30 * time.Second

// Class buckets errors the same way Telegram / Lark adapters do so
// the worker's retry / DLQ decision stays uniform across platforms.
type Class int

const (
	Transient       Class = iota // retry with backoff
	PermanentClient              // config / credentials — do NOT retry
)

func (c Class) String() string {
	switch c {
	case Transient:
		return "transient"
	case PermanentClient:
		return "permanent_client"
	default:
		return "unknown"
	}
}

// APIError carries the structured SMTP failure for the worker. Mirrors
// the tg / lark error shape so the worker doesn't need a switch on the
// concrete error type.
type APIError struct {
	Class       Class
	Code        int    // SMTP reply code, when one was seen
	Description string // operator-facing summary, never includes credentials
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil smtp.APIError>"
	}
	return fmt.Sprintf("smtp api error class=%s code=%d desc=%s", e.Class, e.Code, e.Description)
}

// IsTransient implements the worker's classification probe.
func (e *APIError) IsTransient() bool { return e != nil && e.Class == Transient }

// AsAPIError unwraps an *APIError from any error chain. Mirrors
// lark.AsAPIError so worker code can branch uniformly.
func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// ErrBadBotToken is returned when the smtp:// pseudo-URL cannot be
// parsed. Distinct sentinel so callers can detect a misconfigured
// row without inspecting the underlying url.Parse error.
var ErrBadBotToken = errors.New("smtp: malformed bot_token (expect smtp://<user>:<pass>@<host>:<port>?...)")

// SMTPTokenPrefix is the scheme exported so the sender_router can do
// a one-line prefix check without importing the package's internals.
const SMTPTokenPrefix = "smtp://"

// Client delivers email via a configured SMTP relay. Stateless — each
// Send dials, authenticates, transmits, and quits.
type Client struct {
	timeout time.Duration
	// dialer is overridable in tests to point at a local net.Listener
	// without going through DNS. Production leaves it nil → net.Dialer
	// with the configured timeout.
	dialer func(network, address string) (net.Conn, error)
}

// New constructs a production-ready Client with DefaultTimeout. Pass a
// non-zero timeout to tighten the cap (tests use 2s).
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{timeout: timeout}
}

// Send delivers one email. botToken carries the full smtp:// pseudo-URL
// the store layer assembles; chatID is the recipient address (or
// comma-separated list); parseMode steers the Content-Type header;
// text is the rendered template body whose first non-empty line is
// used as the Subject.
//
// The returned int64 is always 0 — SMTP has no per-message id we can
// surface back to the worker. The signature matches domain.Sender so
// the router can dispatch without a type switch.
func (c *Client) Send(ctx context.Context, botToken, chatID, parseMode, text string) (int64, error) {
	cfg, err := parseToken(botToken)
	if err != nil {
		return 0, err
	}
	rcpts, err := parseRecipients(chatID)
	if err != nil {
		return 0, &APIError{Class: PermanentClient, Description: err.Error()}
	}
	subject, body := splitSubjectBody(text)
	from := cfg.from
	if from == "" {
		from = cfg.username
	}
	msg := buildMessage(from, rcpts, subject, body, parseMode)
	return 0, c.deliver(ctx, cfg, from, rcpts, msg)
}

// smtpConfig is the parsed shape of botToken. Kept private — callers
// only ever feed in the raw pseudo-URL.
type smtpConfig struct {
	host     string
	port     int
	username string
	password string
	from     string
	useTLS   bool
}

func parseToken(botToken string) (*smtpConfig, error) {
	if !strings.HasPrefix(botToken, SMTPTokenPrefix) {
		return nil, ErrBadBotToken
	}
	u, err := url.Parse(botToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadBotToken, err)
	}
	if u.Scheme != "smtp" || u.Host == "" {
		return nil, ErrBadBotToken
	}
	host := u.Hostname()
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 587
	}
	if host == "" {
		return nil, ErrBadBotToken
	}
	user := u.User.Username()
	if user == "" {
		return nil, ErrBadBotToken
	}
	pass, _ := u.User.Password()
	q := u.Query()
	cfg := &smtpConfig{
		host:     host,
		port:     port,
		username: user,
		password: pass,
		from:     q.Get("from"),
		useTLS:   q.Get("tls") != "0", // default ON
	}
	return cfg, nil
}

// parseRecipients splits the comma-separated chat_id into a clean
// recipient list. Empty or syntactically-broken addresses fail loudly
// so a typo doesn't silently send to a partial list.
func parseRecipients(chatID string) ([]string, error) {
	if strings.TrimSpace(chatID) == "" {
		return nil, errors.New("smtp: chat_id (recipient list) is empty")
	}
	raw := strings.Split(chatID, ",")
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		addr := strings.TrimSpace(r)
		if addr == "" {
			continue
		}
		// Minimal validation: must contain exactly one '@' with content
		// on both sides. We don't try to mimic RFC 5322 — the SMTP
		// server will reject malformed RCPT TO: with a 5xx that the
		// caller surfaces as PermanentClient.
		if at := strings.IndexByte(addr, '@'); at <= 0 || at == len(addr)-1 {
			return nil, fmt.Errorf("smtp: invalid recipient address %q", addr)
		}
		out = append(out, addr)
	}
	if len(out) == 0 {
		return nil, errors.New("smtp: chat_id produced no recipients")
	}
	return out, nil
}

// splitSubjectBody extracts the Subject: from the rendered template
// body. The contract is documented in apidocs:
//   - First non-empty line → Subject
//   - Remaining lines (after the first "\n\n", if any) → body
//   - If the body has no blank-line separator, the whole rendered
//     text becomes the body and Subject falls back to a generic
//     "[PulseGuard alert]" label.
//
// This sidesteps adding a schema field for subject while keeping
// templates ergonomic: operators write a one-line headline + blank
// line + detail block (the natural Markdown structure).
func splitSubjectBody(text string) (subject, body string) {
	trimmed := strings.TrimLeft(text, "\r\n\t ")
	if trimmed == "" {
		return "[PulseGuard alert]", ""
	}
	// Split at the first blank line.
	if idx := strings.Index(trimmed, "\n\n"); idx >= 0 {
		first := strings.TrimSpace(trimmed[:idx])
		rest := strings.TrimLeft(trimmed[idx+2:], "\n")
		if first == "" {
			return "[PulseGuard alert]", rest
		}
		return first, rest
	}
	// No blank line — first line is subject, rest is body. Even when
	// there is no rest, the operator still gets a sensibly-titled
	// email (the body shows the title again, which is fine).
	if nl := strings.IndexByte(trimmed, '\n'); nl >= 0 {
		first := strings.TrimSpace(trimmed[:nl])
		rest := strings.TrimLeft(trimmed[nl+1:], "\n")
		return first, rest
	}
	// Single line — treat as subject; body left empty.
	return strings.TrimSpace(trimmed), ""
}

// buildMessage assembles the RFC 5322-ish payload. Headers are minimal
// (From, To, Subject, MIME-Version, Content-Type, Date) — production
// relays add the rest. Body lines starting with "." are dot-stuffed
// per RFC 5321 §4.5.2 so a template that emits ".\n" doesn't terminate
// the DATA stream prematurely.
func buildMessage(from string, rcpts []string, subject, body, parseMode string) []byte {
	contentType := "text/plain; charset=\"UTF-8\""
	if strings.EqualFold(parseMode, "HTML") {
		contentType = "text/html; charset=\"UTF-8\""
	}
	var b strings.Builder
	b.WriteString("From: ")
	b.WriteString(from)
	b.WriteString("\r\n")
	b.WriteString("To: ")
	b.WriteString(strings.Join(rcpts, ", "))
	b.WriteString("\r\n")
	b.WriteString("Subject: ")
	b.WriteString(encodeSubject(subject))
	b.WriteString("\r\n")
	b.WriteString("Date: ")
	b.WriteString(time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: ")
	b.WriteString(contentType)
	b.WriteString("\r\n\r\n")
	// Dot-stuff body so a literal ".\r\n" in the rendered template
	// cannot terminate the DATA stream early.
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, ".") {
			b.WriteByte('.')
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

// encodeSubject wraps non-ASCII subjects in RFC 2047 MIME-Word "B"
// encoding so Chinese / emoji titles render correctly across mail
// clients. ASCII-only subjects skip the wrapping for legibility.
func encodeSubject(s string) string {
	for _, r := range s {
		if r > 127 {
			// Base64-encode the whole subject in one MIME word. Mail
			// clients are required to support arbitrary-length B-words
			// since RFC 2047, though folding at 75 chars is the
			// recommendation. For typical alert subject lengths (<100
			// chars) the single-word form is fine.
			return mimeWordEncode(s)
		}
	}
	return s
}

func mimeWordEncode(s string) string {
	// Hand-roll base64 instead of importing encoding/base64 just for
	// this — actually no, base64 is in stdlib, just use it:
	return "=?UTF-8?B?" + base64Std(s) + "?="
}

// base64Std encodes s into standard base64. Tiny shim so the import
// list at the top of the file stays focused.
func base64Std(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// stdEncode kept as a thin wrapper to match the indirect call from
// base64Std (preserves source-level readability if we ever swap the
// encoding implementation).
func stdEncode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// deliver performs the actual SMTP transaction. Returns nil on success,
// *APIError on classified failure, raw error on unclassifiable I/O
// errors (the worker treats those as transient).
func (c *Client) deliver(ctx context.Context, cfg *smtpConfig, from string, rcpts []string, msg []byte) error {
	addr := net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port))
	// Cancellable dial via context: use ctx deadline if shorter than
	// our configured timeout. The worker passes a per-attempt context
	// scoped by config.telegram.http_timeout normally — for SMTP we
	// fall back to our own cap when the parent has none.
	d := net.Dialer{Timeout: c.timeout}
	if dl, ok := ctx.Deadline(); ok {
		// Shrink our timeout to match the context's remaining budget.
		if rem := time.Until(dl); rem > 0 && rem < d.Timeout {
			d.Timeout = rem
		}
	}

	var conn net.Conn
	var err error
	if c.dialer != nil {
		conn, err = c.dialer("tcp", addr)
	} else if cfg.port == 465 && cfg.useTLS {
		// Implicit TLS (SMTPS). Wrap the dial in tls.DialWithDialer
		// so cert validation + handshake share the same timeout.
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: cfg.host})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return &APIError{Class: Transient, Description: "smtp dial: " + err.Error()}
	}
	// Cancel the conn when the caller's ctx ends so a hung server
	// doesn't block past the worker's deadline.
	go func() {
		<-ctx.Done()
		_ = conn.SetDeadline(time.Now())
	}()

	client, err := smtp.NewClient(conn, cfg.host)
	if err != nil {
		_ = conn.Close()
		return &APIError{Class: Transient, Description: "smtp greeting: " + err.Error()}
	}
	defer func() { _ = client.Close() }()

	// STARTTLS for port 587 / etc. when use_tls is on. Skip when
	// implicit TLS was already established at dial time.
	if cfg.useTLS && cfg.port != 465 {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return &APIError{Class: PermanentClient, Description: "smtp: server does not support STARTTLS but use_tls=true"}
		}
		if err := client.StartTLS(&tls.Config{ServerName: cfg.host}); err != nil {
			return &APIError{Class: Transient, Description: "smtp starttls: " + err.Error()}
		}
	}

	auth := smtp.PlainAuth("", cfg.username, cfg.password, cfg.host)
	if err := client.Auth(auth); err != nil {
		return classifyAuthError(err)
	}
	if err := client.Mail(from); err != nil {
		return classifyServerError("MAIL FROM", err)
	}
	for _, r := range rcpts {
		if err := client.Rcpt(r); err != nil {
			return classifyServerError("RCPT TO", err)
		}
	}
	wc, err := client.Data()
	if err != nil {
		return classifyServerError("DATA", err)
	}
	if _, err := wc.Write(msg); err != nil {
		_ = wc.Close()
		return &APIError{Class: Transient, Description: "smtp data write: " + err.Error()}
	}
	if err := wc.Close(); err != nil {
		return classifyServerError("DATA close", err)
	}
	_ = client.Quit()
	return nil
}

// classifyAuthError maps AUTH failures to PermanentClient — the
// credentials are wrong and a retry will not help.
func classifyAuthError(err error) error {
	if err == nil {
		return nil
	}
	return &APIError{
		Class:       PermanentClient,
		Code:        parseSMTPCode(err),
		Description: "smtp auth failed: " + err.Error(),
	}
}

// classifyServerError reads the SMTP reply code (4xx → transient,
// 5xx → permanent). 421 (service not available) and 451/452 transient
// codes are common during relay maintenance; 550 (mailbox not found)
// and 553 (bad address) are permanent.
func classifyServerError(stage string, err error) error {
	if err == nil {
		return nil
	}
	code := parseSMTPCode(err)
	class := Transient
	if code >= 500 && code < 600 {
		// Most 5xx are permanent; we treat 500-509 (syntax) and
		// 530-559 (auth/policy/mailbox) as PermanentClient. 521 / 554
		// (transaction failed) classify as transient since they often
		// stem from greylisting or rate limits.
		switch code {
		case 521, 554:
			class = Transient
		default:
			class = PermanentClient
		}
	}
	return &APIError{
		Class:       class,
		Code:        code,
		Description: stage + ": " + err.Error(),
	}
}

// parseSMTPCode extracts the leading 3-digit reply code from an error
// returned by net/smtp. Returns 0 when no recognisable code is
// present (network errors, parse failures, etc.).
func parseSMTPCode(err error) int {
	if err == nil {
		return 0
	}
	s := err.Error()
	if len(s) < 3 {
		return 0
	}
	n, perr := strconv.Atoi(s[:3])
	if perr != nil {
		return 0
	}
	return n
}
