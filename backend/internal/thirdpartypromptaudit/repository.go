package thirdpartypromptaudit

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// ErrLeaseLost 防止过期执行者继续写入分类或处置。
var ErrLeaseLost = errors.New("审核任务租约已失效")

// ErrAuditPaused 表示当前配置已撤销尚未发起的审核调用许可。
var ErrAuditPaused = errors.New("审核已暂停，等待重新启用")

// ErrUserExcluded 表示用户在首个模型调用前已被设置为不审核。
var ErrUserExcluded = errors.New("用户当前已设置为不审核")

// ErrNotFound 表示请求的本模块业务记录不存在。
var ErrNotFound = errors.New("审核记录不存在")

// leaseDuration 为可续租任务提供进程故障恢复窗口。
const leaseDuration = 30 * time.Second

type Repository struct {
	db    *sql.DB
	redis *RedisStore
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// SetRedisStore 装配回合键、响应映射和统计缓存。
func (r *Repository) SetRedisStore(store *RedisStore) { r.redis = store }

func checkLeaseUpdate(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrLeaseLost
	}
	return nil
}

func interval(duration time.Duration) string { return fmt.Sprintf("%.6f seconds", duration.Seconds()) }

// encodeStoredText 可逆保存 NUL 和非 UTF-8 外部响应，避免数据库拒绝或静默替换字符。
func encodeStoredText(value string) string {
	encoding, data := "utf8", value
	if !utf8.ValidString(value) {
		encoding, data = "base64", base64.StdEncoding.EncodeToString([]byte(value))
	}
	raw, _ := json.Marshal(struct {
		Encoding string `json:"encoding"`
		Data     string `json:"data"`
	}{encoding, data})
	return string(raw)
}

func decodeStoredText(value string) string {
	var encoded struct {
		Encoding string `json:"encoding"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal([]byte(value), &encoded); err != nil {
		return value
	}
	if encoded.Encoding == "utf8" {
		return encoded.Data
	}
	raw, err := base64.StdEncoding.DecodeString(encoded.Data)
	if err != nil {
		return value
	}
	return string(raw)
}
