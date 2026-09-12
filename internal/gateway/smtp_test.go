package gateway

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// The account-notification tests drive the outbox with a stub transport, so the
// real SMTP send path — message framing + the smtp.SendMail conversation — has no
// coverage. These tests stand up a minimal in-process SMTP server and assert that
// smtpNotifier speaks to it correctly, which is what "reset emails actually send"
// depends on.

// capturedMail is what the fake server recorded from one delivery.
type capturedMail struct {
	from string
	to   []string
	data string
}

// fakeSMTP is a bare, unauthenticated SMTP sink for one message. It advertises
// neither STARTTLS nor AUTH, so smtp.SendMail (with nil auth) sends in the clear —
// enough to verify the envelope and the message bytes.
func fakeSMTP(t *testing.T) (addr string, got <-chan capturedMail) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	out := make(chan capturedMail, 1)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(conn)
		w := func(s string) { _, _ = conn.Write([]byte(s)) }

		w("220 fake ESMTP\r\n")
		var mail capturedMail
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				// Advertise nothing extra: no STARTTLS, no AUTH.
				w("250-fake\r\n250 OK\r\n")
			case strings.HasPrefix(cmd, "MAIL FROM:"):
				mail.from = addrArg(line)
				w("250 OK\r\n")
			case strings.HasPrefix(cmd, "RCPT TO:"):
				mail.to = append(mail.to, addrArg(line))
				w("250 OK\r\n")
			case cmd == "DATA":
				w("354 end with .\r\n")
				var b strings.Builder
				for {
					dl, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimRight(dl, "\r\n") == "." {
						break
					}
					b.WriteString(dl)
				}
				mail.data = b.String()
				w("250 OK\r\n")
			case cmd == "QUIT":
				w("221 Bye\r\n")
				out <- mail
				return
			default:
				w("250 OK\r\n")
			}
		}
	}()

	return ln.Addr().String(), out
}

// addrArg pulls the address out of "MAIL FROM:<a@b>" / "RCPT TO:<a@b>".
func addrArg(line string) string {
	i, j := strings.IndexByte(line, '<'), strings.IndexByte(line, '>')
	if i >= 0 && j > i {
		return line[i+1 : j]
	}
	return strings.TrimSpace(line[strings.IndexByte(line, ':')+1:])
}

func TestSMTPNotifier_SendsWellFormedMessage(t *testing.T) {
	addr, got := fakeSMTP(t)
	host, port := splitHostPort(t, addr)

	n := NewSMTPNotifier(host, port, "noreply@dcms.test", "", "") // unauthenticated relay
	err := n.Notify(context.Background(), Notification{
		To:   "user@example.com",
		Kind: "password_reset",
		Link: "https://app.example/reset?token=abc123",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	select {
	case m := <-got:
		if m.from != "noreply@dcms.test" {
			t.Errorf("envelope from = %q, want noreply@dcms.test", m.from)
		}
		if len(m.to) != 1 || m.to[0] != "user@example.com" {
			t.Errorf("envelope rcpt = %v, want [user@example.com]", m.to)
		}
		// Headers and the reset link must be present in the DATA blob.
		for _, want := range []string{
			"From: noreply@dcms.test",
			"To: user@example.com",
			"Subject: Reset your password",
			"Content-Type: text/plain; charset=UTF-8",
			"https://app.example/reset?token=abc123",
		} {
			if !strings.Contains(m.data, want) {
				t.Errorf("message body missing %q\n---\n%s", want, m.data)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the SMTP server to receive the message")
	}
}

// TestSMTPNotifier_OutboxToWire proves the whole account-email chain end to end
// against a real SMTP server: a queued notification is drained by the delivery
// worker, sent over SMTP, and its row deleted on success (so the reset link does
// not linger). The other tests cover the transport and the outbox separately;
// this pins that they compose.
func TestSMTPNotifier_OutboxToWire(t *testing.T) {
	addr, got := fakeSMTP(t)
	host, port := splitHostPort(t, addr)

	srv, db := buildNotifyServer(t, NewSMTPNotifier(host, port, "noreply@dcms.test", "", ""))
	ctx := context.Background()

	link := "https://app.example/reset?token=e2e-token"
	if err := srv.enqueueNotification(ctx, Notification{To: "u@example.com", Kind: "password_reset", Link: link}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	srv.deliverNotifications(ctx)

	select {
	case m := <-got:
		if !strings.Contains(m.data, link) {
			t.Errorf("delivered message missing the reset link:\n%s", m.data)
		}
		if m.to[0] != "u@example.com" {
			t.Errorf("rcpt = %v, want u@example.com", m.to)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not deliver the queued notification over SMTP")
	}
	// Delivered rows are removed so the raw link does not linger.
	if n := len(allNotifications(t, db)); n != 0 {
		t.Fatalf("delivered notification should be deleted, %d remain", n)
	}
}

// splitHostPort parses "127.0.0.1:PORT" into host + int port.
func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return host, port
}
