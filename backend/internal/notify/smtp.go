package notify

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sub2api-enhance/internal/config"
	"time"
)

// ErrNotConfigured 区分未配置通知与可重试的投递故障。
var ErrNotConfigured = errors.New("尚未配置 SMTP 通知")

type Sender interface {
	SendEmail(context.Context, string, string, string) error
}
type SMTP struct {
	config *config.Config
	db     *sql.DB
}

func NewSMTP(db *sql.DB, c *config.Config) *SMTP { return &SMTP{config: c, db: db} }
func (s *SMTP) SendEmail(ctx context.Context, to, subject, body string) error {
	c := s.config
	if s.db != nil {
		var host, port, user, password, from, fromName, useTLS string
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(value) FILTER (WHERE key='smtp_host'),''),COALESCE(MAX(value) FILTER (WHERE key='smtp_port'),''),COALESCE(MAX(value) FILTER (WHERE key='smtp_username'),''),COALESCE(MAX(value) FILTER (WHERE key='smtp_password'),''),COALESCE(MAX(value) FILTER (WHERE key='smtp_from'),''),COALESCE(MAX(value) FILTER (WHERE key='smtp_from_name'),''),COALESCE(MAX(value) FILTER (WHERE key='smtp_use_tls'),'false') FROM public.settings`).Scan(&host, &port, &user, &password, &from, &fromName, &useTLS); err == nil && host != "" {
			if n, err := strconv.Atoi(port); err == nil && n > 0 {
				port = strconv.Itoa(n)
			}
			c = &config.Config{SMTPHost: host, SMTPPort: port, SMTPUser: user, SMTPPassword: password, SMTPFrom: from, SMTPFromName: fromName, SMTPUseTLS: useTLS == "true"}
		}
	}
	if c == nil || c.SMTPHost == "" {
		return ErrNotConfigured
	}
	from, err := mail.ParseAddress(c.SMTPFrom)
	if err != nil {
		return errors.New("SMTP 发件人地址无效")
	}
	recipient, err := mail.ParseAddress(to)
	if err != nil {
		return err
	}
	if strings.ContainsAny(subject, "\r\n") {
		return errors.New("邮件标题不能包含换行")
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	address := net.JoinHostPort(c.SMTPHost, c.SMTPPort)
	tlsConfig := &tls.Config{ServerName: c.SMTPHost, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if c.SMTPPort == "465" || c.SMTPUseTLS {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = d.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	client, err := smtp.NewClient(conn, c.SMTPHost)
	if err != nil {
		return err
	}
	defer client.Close()
	if c.SMTPPort != "465" && !c.SMTPUseTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP 服务必须支持 STARTTLS")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if c.SMTPUser != "" {
		if err := client.Auth(smtp.PlainAuth("", c.SMTPUser, c.SMTPPassword, c.SMTPHost)); err != nil {
			return err
		}
	}
	if err := client.Mail(from.Address); err != nil {
		return err
	}
	if err := client.Rcpt(recipient.Address); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	fromHeader := from.String()
	if c.SMTPFromName != "" {
		fromHeader = (&mail.Address{Name: c.SMTPFromName, Address: from.Address}).String()
	}
	_, err = fmt.Fprintf(writer, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s", fromHeader, recipient.String(), mime.QEncoding.Encode("utf-8", subject), body)
	if err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}
