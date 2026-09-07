package migrations

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"testing/fstest"
)

func TestExistingMigrationMustHaveIdenticalChecksum(t *testing.T) {
	for _, matches := range []bool{true, false} {
		t.Run(fmt.Sprint(matches), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			db.SetMaxOpenConns(1)
			mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
			mock.ExpectExec("CREATE SCHEMA").WillReturnResult(sqlmock.NewResult(0, 0))
			entries, err := files.ReadDir(".")
			require.NoError(t, err)
			for _, e := range entries {
				raw, err := files.ReadFile(e.Name())
				require.NoError(t, err)
				checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(string(raw)))))
				if !matches {
					checksum = "changed"
				}
				mock.ExpectQuery("SELECT checksum").WithArgs(e.Name()).WillReturnRows(sqlmock.NewRows([]string{"checksum"}).AddRow(checksum))
				if !matches {
					break
				}
			}
			mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
			err = Run(context.Background(), db)
			if matches {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "迁移内容发生变化")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
func TestLaterMigrationFailurePreservesEarlierCommitAndRollsBackOnlyFailedFile(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectExec("CREATE SCHEMA").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT checksum").WithArgs("001.sql").WillReturnRows(sqlmock.NewRows([]string{"checksum"}))
	mock.ExpectBegin()
	mock.ExpectExec("CREATE TABLE first").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO .*schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT checksum").WithArgs("002.sql").WillReturnRows(sqlmock.NewRows([]string{"checksum"}))
	mock.ExpectBegin()
	mock.ExpectExec("CREATE TABLE second").WillReturnError(errors.New("permission denied"))
	mock.ExpectRollback()
	mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
	err = runFS(context.Background(), db, fstest.MapFS{"001.sql": &fstest.MapFile{Data: []byte("CREATE TABLE first(id int);")}, "002.sql": &fstest.MapFile{Data: []byte("CREATE TABLE second(id int);")}})
	require.ErrorContains(t, err, "002.sql")
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestNonTransactionalMigrationMustBeReplayableConcurrentIndexOnly(t *testing.T) {
	ok, err := validateMigrationExecutionMode("003_notx.sql", "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx ON sub2api_enhance.jobs(id);")
	require.NoError(t, err)
	require.True(t, ok)
	for _, sql := range []string{"BEGIN; CREATE INDEX CONCURRENTLY idx ON t(id);", "UPDATE t SET id=1;", "CREATE INDEX CONCURRENTLY idx ON t(id);"} {
		_, err := validateMigrationExecutionMode("003_notx.sql", sql)
		require.Error(t, err)
	}
	_, err = validateMigrationExecutionMode("003.sql", "CREATE INDEX CONCURRENTLY idx ON t(id);")
	require.Error(t, err)
}
