package thirdpartypromptaudit

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestListAuditUsersIncludesAdministratorsAndEnforcementState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT u.id,u.username,u.email,u.role,u.status").WillReturnRows(sqlmock.NewRows([]string{"id", "username", "email", "role", "status", "count", "reset_at", "pending"}).
		AddRow(1, "root", "root@example.invalid", "admin", "active", 0, nil, false).
		AddRow(2, "user", "user@example.invalid", "user", "disabled", 4, nil, true))
	users, err := NewRepository(db).ListAuditUsers(context.Background())
	require.NoError(t, err)
	require.Len(t, users, 2)
	require.Equal(t, "admin", users[0].Role)
	require.EqualValues(t, 4, users[1].DisableViolationCount)
	require.True(t, users[1].ActionPending)
	require.NoError(t, mock.ExpectationsWereMet())
}
