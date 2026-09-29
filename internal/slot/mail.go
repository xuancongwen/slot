package slot

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Mailer sends a plain-text email.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// SMTP sends mail through a submission server, always over TLS: implicit TLS on
// port 465, STARTTLS otherwise. Credentials are never sent in the clear.
type SMTP struct {
	Host, Port, Username, Password string
	From                           *mail.Address
	// tls overrides the client TLS settings, so tests can trust their own server.
	tls *tls.Config
}

func (s *SMTP) Send(ctx context.Context, to, subject, body string) error {
	cfg := &tls.Config{ServerName: s.Host}
	if s.tls != nil {
		cfg = s.tls
	}
	addr := net.JoinHostPort(s.Host, s.Port)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if s.Port == "465" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: cfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connecting to SMTP server: %w", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	if err = conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return err
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("greeting SMTP server: %w", err)
	}
	defer c.Close()
	if s.Port != "465" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("SMTP server does not offer STARTTLS")
		}
		if err = c.StartTLS(cfg); err != nil {
			return fmt.Errorf("starting TLS: %w", err)
		}
	}
	if s.Username != "" {
		if err = c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("authenticating: %w", err)
		}
	}
	if err = c.Mail(s.From.Address); err != nil {
		return fmt.Errorf("setting sender: %w", err)
	}
	if err = c.Rcpt(to); err != nil {
		return fmt.Errorf("setting recipient: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("starting message: %w", err)
	}
	if _, err = w.Write(message(s.From, to, subject, body, time.Now())); err != nil {
		return fmt.Errorf("writing message: %w", err)
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("sending message: %w", err)
	}
	return c.Quit()
}

// message formats a UTF-8 plain-text email. Callers pass a validated recipient
// address; the subject is encoded so it cannot inject headers.
func message(from *mail.Address, to, subject, body string, now time.Time) []byte {
	domain := from.Address[strings.LastIndexByte(from.Address, '@')+1:]
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n",
		from.String(), to, mime.QEncoding.Encode("utf-8", subject), now.Format(time.RFC1123Z), randomHex(16), domain)
	// Writes to a bytes.Buffer cannot fail.
	qp := quotedprintable.NewWriter(&b)
	qp.Write([]byte(body))
	qp.Close()
	return b.Bytes()
}
