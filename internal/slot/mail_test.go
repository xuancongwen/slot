package slot

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// smtpSession is what the fake server saw.
type smtpSession struct {
	authOverTLS, authInClear bool
	data                     string
}

// fakeSMTP serves one SMTP session, offering STARTTLS only when startTLS is set.
func fakeSMTP(ln net.Listener, cfg *tls.Config, startTLS bool, done chan<- smtpSession) {
	var s smtpSession
	defer func() { done <- s }()
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer func() { conn.Close() }()
	tp := textproto.NewConn(conn)
	secure := false
	tp.PrintfLine("220 fake ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, _, _ := strings.Cut(strings.ToUpper(line), " ")
		switch verb {
		case "EHLO":
			tp.PrintfLine("250-fake")
			if startTLS && !secure {
				tp.PrintfLine("250-STARTTLS")
			}
			tp.PrintfLine("250 AUTH PLAIN")
		case "STARTTLS":
			tp.PrintfLine("220 ready")
			tc := tls.Server(conn, cfg)
			if tc.Handshake() != nil {
				return
			}
			conn, tp, secure = tc, textproto.NewConn(tc), true
		case "AUTH":
			s.authOverTLS, s.authInClear = secure, !secure
			tp.PrintfLine("235 ok")
		case "MAIL", "RCPT", "RSET", "NOOP":
			tp.PrintfLine("250 ok")
		case "DATA":
			tp.PrintfLine("354 go ahead")
			lines, err := tp.ReadDotLines()
			if err != nil {
				return
			}
			s.data = strings.Join(lines, "\n")
			tp.PrintfLine("250 queued")
		case "QUIT":
			tp.PrintfLine("221 bye")
			return
		default:
			tp.PrintfLine("502 unknown")
		}
	}
}

func TestSMTPSend(t *testing.T) {
	// Borrow httptest's certificate, which is valid for 127.0.0.1.
	https := httptest.NewTLSServer(nil)
	defer https.Close()
	roots := x509.NewCertPool()
	roots.AddCert(https.Certificate())
	for _, tc := range []struct {
		name     string
		startTLS bool
		wantErr  string
	}{
		{"STARTTLS", true, ""},
		{"no STARTTLS", false, "STARTTLS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan smtpSession, 1)
			go fakeSMTP(ln, &tls.Config{Certificates: https.TLS.Certificates}, tc.startTLS, done)
			host, port, _ := net.SplitHostPort(ln.Addr().String())
			s := &SMTP{Host: host, Port: port, Username: "me", Password: "secret", From: &mail.Address{Name: "Slot", Address: "book@example.com"}, tls: &tls.Config{RootCAs: roots, ServerName: host}}
			err = s.Send(context.Background(), "guest@example.com", "Confirm your booking", "Open this link:\n\nhttps://book.example.com/manage/abc\n")
			got := <-done
			if got.authInClear {
				t.Fatal("credentials sent without TLS")
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want mention of %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !got.authOverTLS {
				t.Fatalf("error %v, authenticated over TLS %v", err, got.authOverTLS)
			}
			for _, want := range []string{"From: \"Slot\" <book@example.com>", "To: guest@example.com", "Subject: Confirm your booking", "https://book.example.com/manage/abc"} {
				if !strings.Contains(got.data, want) {
					t.Errorf("message missing %q:\n%s", want, got.data)
				}
			}
		})
	}
}

func TestMessageEncodesSubject(t *testing.T) {
	m := string(message(&mail.Address{Address: "book@example.com"}, "guest@example.com", "Meet Zoë\r\nBcc: victim@example.com", "Hi\n", time.Now()))
	headers, body, _ := strings.Cut(m, "\r\n\r\n")
	if strings.Contains(headers, "\r\nBcc:") {
		t.Fatalf("subject injected a header:\n%s", headers)
	}
	if body != "Hi\r\n" {
		t.Fatalf("body %q", body)
	}
}
