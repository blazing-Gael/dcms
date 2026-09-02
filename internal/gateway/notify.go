package gateway

import (
	"context"
	"fmt"
	"log/slog"
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
	from string
	auth smtp.Auth
}

// NewSMTPNotifier builds an SMTP-backed Notifier. When username is set, it
// authenticates with PLAIN (over the STARTTLS connection smtp.SendMail
// negotiates). Port 0 defaults to 587.
func NewSMTPNotifier(host string, port int, from, username, password string) Notifier {
	if port == 0 {
		port = 587
	}
	var auth smtp.Auth
	if username != "" {
		auth = smtp.PlainAuth("", username, password, host)
	}
	return smtpNotifier{addr: fmt.Sprintf("%s:%d", host, port), from: from, auth: auth}
}

func (n smtpNotifier) Notify(_ context.Context, msg Notification) error {
	subject, body := msg.subjectBody()
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", n.from)
	fmt.Fprintf(&b, "To: %s\r\n", msg.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(body)
	return smtp.SendMail(n.addr, n.auth, n.from, []string{msg.To}, []byte(b.String()))
}
