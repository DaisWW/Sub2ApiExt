package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
)

func parseMailbox(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("mailbox is empty or contains a newline")
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address == "" {
		return "", fmt.Errorf("invalid mailbox %q", value)
	}
	return address.Address, nil
}

func (s *EmailSender) send(ctx context.Context, message []byte) error {
	address := net.JoinHostPort(s.config.Host, strconv.Itoa(s.config.Port))
	dialer := &net.Dialer{Timeout: s.config.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("dial smtp server: %w", err)
	}
	deadline := time.Now().Add(s.config.Timeout)
	_ = conn.SetDeadline(deadline)
	defer conn.Close()

	client, err := openSMTPClient(ctx, conn, s.config)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := client.Auth(smtp.PlainAuth("", s.config.Username, s.config.Password, s.config.Host)); err != nil {
		return fmt.Errorf("smtp authentication failed; check the SMTP authorization code")
	}
	if err := sendSMTPMessage(client, s.config, message); err != nil {
		return err
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("finish smtp session: %w", err)
	}
	return nil
}

func openSMTPClient(ctx context.Context, conn net.Conn, cfg config.EmailConfig) (*smtp.Client, error) {
	var client *smtp.Client
	var err error
	if cfg.Security == "implicit_tls" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("smtp TLS handshake: %w", err)
		}
		client, err = smtp.NewClient(tlsConn, cfg.Host)
	} else {
		client, err = smtp.NewClient(conn, cfg.Host)
		if err == nil {
			if ok, _ := client.Extension("STARTTLS"); !ok {
				_ = client.Close()
				return nil, fmt.Errorf("smtp server does not support STARTTLS")
			}
			err = client.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
		}
	}
	if err != nil {
		return nil, fmt.Errorf("open smtp client: %w", err)
	}
	return client, nil
}

func sendSMTPMessage(client *smtp.Client, cfg config.EmailConfig, message []byte) error {
	from, err := parseMailbox(cfg.From)
	if err != nil {
		return err
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("smtp MAIL FROM failed: %w", err)
	}
	for _, recipient := range cfg.To {
		address, err := parseMailbox(recipient)
		if err != nil {
			return err
		}
		if err := client.Rcpt(address); err != nil {
			return fmt.Errorf("smtp RCPT TO failed: %w", err)
		}
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA failed: %w", err)
	}
	if _, err := writer.Write(message); err != nil {
		_ = writer.Close()
		return fmt.Errorf("write email body: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close email body: %w", err)
	}
	return nil
}
