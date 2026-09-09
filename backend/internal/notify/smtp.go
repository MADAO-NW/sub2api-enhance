package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"sub2api-enhance/internal/config"
	"time"
)

// ErrNotConfigured 区分未配置通知与可重试的投递故障。
var ErrNotConfigured = errors.New("尚未配置 SMTP 通知")

type Sender interface {
	SendEmail(context.Context, string, string, string) error
}
type SMTP struct{ config *config.Config }

func NewSMTP(c *config.Config) *SMTP { return &SMTP{config: c} }
func (s *SMTP) SendEmail(ctx context.Context, to, subject, body string) error {
	c := s.config
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
	if c.SMTPPort == "465" {
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
	if c.SMTPPort != "465" {
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
	_, err = fmt.Fprintf(writer, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s", from.String(), recipient.String(), mime.QEncoding.Encode("utf-8", subject), body)
	if err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}
