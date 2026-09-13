package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	EnhanceRedisURL, QuotaRedisURL, QuotaTimezone, QuotaFlusherEnabled      string
	Listen, DatabaseURL, OfficialURL, PublicOrigin, AdminKey, EncryptionKey string
	TrustedProxies                                                          []string
	DatabaseConnections, IngressConnections                                 int
	SMTPHost, SMTPPort, SMTPUser, SMTPPassword, SMTPFrom, SMTPFromName      string
	SMTPUseTLS                                                              bool
	AuditCacheTTL                                                           time.Duration
}

func Load() (*Config, error) {
	c := &Config{Listen: os.Getenv("ENHANCE_LISTEN"), DatabaseURL: os.Getenv("ENHANCE_DATABASE_URL"), OfficialURL: os.Getenv("SUB2API_INTERNAL_URL"), PublicOrigin: os.Getenv("ENHANCE_PUBLIC_ORIGIN"), AdminKey: os.Getenv("SUB2API_ADMIN_API_KEY"), EncryptionKey: os.Getenv("ENHANCE_ENCRYPTION_KEY"), SMTPHost: os.Getenv("SMTP_HOST"), SMTPPort: os.Getenv("SMTP_PORT"), SMTPUser: os.Getenv("SMTP_USER"), SMTPPassword: os.Getenv("SMTP_PASSWORD"), SMTPFrom: os.Getenv("SMTP_FROM"), SMTPFromName: os.Getenv("SMTP_FROM_NAME"), SMTPUseTLS: strings.EqualFold(os.Getenv("SMTP_USE_TLS"), "true"), EnhanceRedisURL: os.Getenv("ENHANCE_REDIS_URL"), QuotaRedisURL: os.Getenv("QUOTA_FOLLOW_REDIS_URL"), QuotaTimezone: os.Getenv("SUB2API_TIMEZONE"), QuotaFlusherEnabled: os.Getenv("SUB2API_USER_PLATFORM_QUOTA_FLUSHER_ENABLED")}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:18081"
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return nil, errors.New("ENHANCE_LISTEN 必须包含地址与端口")
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("必须配置 ENHANCE_DATABASE_URL")
	}
	if c.EnhanceRedisURL == "" {
		return nil, errors.New("必须配置 ENHANCE_REDIS_URL")
	}
	auditCacheTTL, err := time.ParseDuration(os.Getenv("ENHANCE_AUDIT_CACHE_TTL"))
	if err != nil || auditCacheTTL <= 0 {
		return nil, errors.New("ENHANCE_AUDIT_CACHE_TTL 必须是正数 Go 时长")
	}
	c.AuditCacheTTL = auditCacheTTL
	for _, v := range []struct{ name, value string }{{"SUB2API_INTERNAL_URL", c.OfficialURL}, {"ENHANCE_PUBLIC_ORIGIN", c.PublicOrigin}} {
		u, err := url.Parse(v.value)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New(v.name + " 必须是无凭据和路径的 HTTP(S) origin")
		}
	}
	origin, _ := url.Parse(c.PublicOrigin)
	if origin.Scheme == "http" {
		host := origin.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("非本机增强页面必须使用 HTTPS")
		}
	}
	c.OfficialURL = strings.TrimRight(c.OfficialURL, "/")
	c.PublicOrigin = strings.TrimRight(c.PublicOrigin, "/")
	if c.OfficialURL == c.PublicOrigin {
		return nil, errors.New("原版内部地址不能指向增强入口，以免递归")
	}
	for _, v := range []struct {
		name   string
		target *int
	}{{"ENHANCE_DB_CONNECTIONS", &c.DatabaseConnections}, {"ENHANCE_INGRESS_CONNECTIONS", &c.IngressConnections}} {
		n, err := strconv.Atoi(os.Getenv(v.name))
		if err != nil || n <= 0 {
			return nil, errors.New(v.name + " 必须配置为正整数")
		}
		*v.target = n
	}
	if value := os.Getenv("ENHANCE_TRUSTED_PROXIES"); value != "" {
		for _, v := range strings.Split(value, ",") {
			if _, _, err := net.ParseCIDR(v); err != nil {
				return nil, errors.New("可信代理必须为 CIDR")
			}
			c.TrustedProxies = append(c.TrustedProxies, v)
		}
	}
	if _, err := NewEncryptor(c); err != nil {
		return nil, err
	}
	if c.SMTPHost != "" {
		if c.SMTPPort == "" || c.SMTPFrom == "" {
			return nil, errors.New("配置 SMTP_HOST 时必须配置 SMTP_PORT 与 SMTP_FROM")
		}
	}
	return c, nil
}

type SecretEncryptor interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
}
type Encryptor struct{ aead cipher.AEAD }

func NewEncryptor(c *Config) (*Encryptor, error) {
	if c.EncryptionKey == "" {
		return &Encryptor{}, nil
	}
	key, err := base64.StdEncoding.DecodeString(c.EncryptionKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("ENHANCE_ENCRYPTION_KEY 必须为 32 字节密钥的 Base64")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	return &Encryptor{aead: aead}, err
}
func (e *Encryptor) Encrypt(value string) (string, error) {
	if e.aead == nil {
		return "", errors.New("未配置增强加密密钥")
	}
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(e.aead.Seal(nonce, nonce, []byte(value), []byte("sub2api++/node-key"))), nil
}
func (e *Encryptor) Decrypt(value string) (string, error) {
	if e.aead == nil {
		return "", errors.New("未配置增强加密密钥")
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) < e.aead.NonceSize() {
		return "", errors.New("节点密文无效")
	}
	n := e.aead.NonceSize()
	plain, err := e.aead.Open(nil, raw[:n], raw[n:], []byte("sub2api++/node-key"))
	return string(plain), err
}
