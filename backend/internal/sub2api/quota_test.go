package sub2api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestQuotaDiscoveryReturnsAllEligibleUsersAndRollsBackIncompleteReads(t *testing.T) {
	for _, readFails := range []bool{false, true} {
		t.Run(fmt.Sprint("readFails=", readFails), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT name FROM public.groups").WithArgs(int64(2)).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("openai"))
			mock.ExpectQuery("SELECT DISTINCT a.id,a.name,a.type").WithArgs(int64(2)).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "type"}).AddRow(1, "account", "oauth"))
			rows := sqlmock.NewRows([]string{"id", "username", "email"})
			for _, id := range []int64{4, 6, 7, 8, 9} {
				rows.AddRow(id, fmt.Sprint("user-", id), fmt.Sprintf("user-%d@example.com", id))
			}
			readErr := errors.New("读取用户失败")
			if readFails {
				rows.RowError(2, readErr)
			}
			// 公开访问分支必须与显式授权并列，且不能再用关系表内连接提前排除用户。
			mock.ExpectQuery(`SELECT DISTINCT u.id,u.username,u.email FROM public.users u JOIN public.user_platform_quotas .*u.role='user' AND u.status='active' AND u.deleted_at IS NULL AND \(\(NOT g.is_exclusive AND NOT u.restrict_public_groups\) OR EXISTS\(SELECT 1 FROM public.user_allowed_groups ug WHERE ug.user_id=u.id AND ug.group_id=g.id\)\) ORDER BY u.id`).
				WithArgs(int64(2)).WillReturnRows(rows).RowsWillBeClosed()
			if !readFails {
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}
			got, err := NewQuotaData(db).Discover(context.Background(), 2)
			if readFails {
				require.ErrorIs(t, err, readErr)
			} else {
				require.NoError(t, err)
				ids := make([]int64, 0, len(got.Users))
				for _, user := range got.Users {
					ids = append(ids, user.ID)
				}
				require.Equal(t, []int64{4, 6, 7, 8, 9}, ids)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQuotaSnapshotRechecksAccessAndPreservesUnavailableAndErrorResults(t *testing.T) {
	readErr := errors.New("配额查询失败")
	for _, queryErr := range []error{nil, sql.ErrNoRows, readErr} {
		t.Run(fmt.Sprint(queryErr), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			query := mock.ExpectQuery(`SELECT u.id.*u.role='user' AND u.status='active' AND u.deleted_at IS NULL AND q.platform='openai' AND q.deleted_at IS NULL AND EXISTS\(SELECT 1 FROM public.groups g WHERE g.id=\$2 AND g.status='active' AND g.deleted_at IS NULL AND \(\(NOT g.is_exclusive AND NOT u.restrict_public_groups\) OR EXISTS\(SELECT 1 FROM public.user_allowed_groups ug WHERE ug.user_id=u.id AND ug.group_id=g.id\)\)\)`).WithArgs(int64(7), int64(2))
			now := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
			if queryErr != nil {
				query.WillReturnError(queryErr)
			} else {
				query.WillReturnRows(sqlmock.NewRows([]string{"id", "username", "daily", "weekly", "daily_start", "weekly_start", "observed_at"}).AddRow(7, "user", "0.0000000001", "500.1234567890", nil, now, now))
			}
			got, err := NewQuotaData(db).Snapshot(context.Background(), 7, 2)
			switch queryErr {
			case nil:
				require.NoError(t, err)
				require.Equal(t, "0.0000000001", got.DailyUsage)
				require.Equal(t, "500.1234567890", got.WeeklyUsage)
				require.Nil(t, got.DailyStart)
			case sql.ErrNoRows:
				require.NoError(t, err)
				require.Nil(t, got)
			default:
				require.ErrorIs(t, err, readErr)
				require.Nil(t, got)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
