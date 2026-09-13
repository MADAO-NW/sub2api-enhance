package notify

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/config"
	"testing"
)

func TestSendEmailUsesSub2APISettingsBeforeEnhanceFallback(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT COALESCE\\(MAX\\(value\\).*FROM public.settings").WillReturnRows(sqlmock.NewRows([]string{"host", "port", "user", "password", "from", "from_name", "use_tls"}).AddRow("127.0.0.1", "1", "sub2api-user", "sub2api-password", "sub2api@example.com", "Sub2API", "false"))
	sender := NewSMTP(db, &config.Config{SMTPHost: "fallback.invalid", SMTPPort: "25", SMTPFrom: "invalid-from"})
	err = sender.SendEmail(context.Background(), "recipient@example.com", "subject", "body")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SMTP 发件人地址无效")
	require.NoError(t, mock.ExpectationsWereMet())
}
