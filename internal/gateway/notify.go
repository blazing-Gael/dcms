package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
)

// Notifier delivers an account notification out-of-band — today a password-reset
// link (ADR-0019 phase 2). It is the seam email delivery plugs into: a dev-log
// impl for local testing, an SMTP impl for production, and (once M-B lands) an
// outbox-backed impl, all without changing the reset flow that calls it.
type Notifier interface {
	Notify(ctx context.Context, msg Notification) error
}

// ConnectionVerifier is an optional Notifier capability: a cheap pre-flight that
// confirms the transport is reachable and (if configured) credentials work,
// without sending a message. A host runs it at startup so a broken mail path is
// caught then rather than only when a user's reset silently fails to arrive.
type ConnectionVerifier interface {
	VerifyConnection(ctx context.Context) error
}

// Notification is one message to send. Link is the action URL the user follows
// (e.g. the password-reset page with the token).
type Notification struct {
	To   string // recipient email
	Kind string // "password_reset" (more later)
	Link string
}

// subjectBody renders a notification to a subject line and a plain-text body.
func (n Notification) subjectBody() (string, string) {
	switch n.Kind {
	case "password_reset":
		return "Reset your password",
			"We received a request to reset your password.\r\n\r\n" +
				"Follow this link to choose a new one:\r\n" + n.Link + "\r\n\r\n" +
				"If you didn't request this, you can ignore this email."
	default:
		return "Notification", n.Link
	}
}

// logNotifier prints the notification (including the link) to the logger. It is
// the zero-config default so password reset works in local dev without SMTP —
// the operator reads the link off the console.
type logNotifier struct{ logger *slog.Logger }

func (n logNotifier) Notify(_ context.Context, msg Notification) error {
	n.logger.Info("account notification (dev — no mailer configured)",
		"to", msg.To, "kind", msg.Kind, "link", msg.Link)
	return nil
}

// smtpNotifier sends via SMTP with STARTTLS (the common submission path, port
// 587). auth may be nil for an unauthenticated relay.
type smtpNotifier struct {
	addr string // host:port
	host string // for TLS ServerName and the auth challenge
	// envelopeFrom is the bare addr-spec used for the SMTP MAIL FROM (which must
	// not carry a display name); headerFrom is the full form for the From: header,
	// so a configured "Agrojatra <noreply@…>" shows a sender name to the reader.
	envelopeFrom string
	headerFrom   string
	auth         smtp.Auth
}

// NewSMTPNotifier builds an SMTP-backed Notifier. When username is set, it
// authenticates with PLAIN (over the STARTTLS connection smtp.SendMail
// negotiates). Port 0 defaults to 587. `from` may be a bare address
// (`noreply@x`) or carry a display name (`Agrojatra <noreply@x>`).
func NewSMTPNotifier(host string, port int, from, username, password string) Notifier {
	if port == 0 {
		port = 587
	}
	var auth smtp.Auth
	if username != "" {
		auth = smtp.PlainAuth("", username, password, host)
	}
	envelope, header := splitFromAddress(from)
	return smtpNotifier{
		addr:         fmt.Sprintf("%s:%d", host, port),
		host:         host,
		envelopeFrom: envelope,
		headerFrom:   header,
		auth:         auth,
	}
}

// splitFromAddress parses a configured From value into the envelope address (a
// bare addr-spec, required by MAIL FROM) and the header form (which keeps any
// display name). "Agrojatra <noreply@x>" → ("noreply@x", `"Agrojatra" <noreply@x>`).
// A value that does not parse is used verbatim for both, preserving the prior
// behaviour rather than failing the send.
func splitFromAddress(from string) (envelope, header string) {
	addr, err := mail.ParseAddress(from)
	if err != nil {
		return from, from
	}
	return addr.Address, addr.String()
}

func (n smtpNotifier) Notify(_ context.Context, msg Notification) error {
	subject, body := msg.subjectBody()
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", n.headerFrom)
	fmt.Fprintf(&b, "To: %s\r\n", msg.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(body)
	return smtp.SendMail(n.addr, n.auth, n.envelopeFrom, []string{msg.To}, []byte(b.String()))
}

// VerifyConnection dials the server and runs the submission handshake up to (but
// not including) sending a message: greeting, EHLO, STARTTLS when offered, and
// AUTH when credentials are configured. It surfaces the common misconfigurations
// — unreachable host, wrong port, bad credentials, TLS problems — at startup.
func (n smtpNotifier) VerifyConnection(ctx context.Context) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", n.addr)
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, n.host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = c.Close() }()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: n.host}); err != nil {
			return err
		}
	}
	if n.auth != nil {
		if err := c.Auth(n.auth); err != nil {
			return err
		}
	}
	return c.Quit()
}
