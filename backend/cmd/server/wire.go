//go:build wireinject

package main

import (
	"database/sql"
	"github.com/google/wire"
	"sub2api-enhance/internal/config"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
)

func initializeCore(db *sql.DB, cfg *config.Config, captures *audit.CaptureStore) (*Core, error) {
	wire.Build(coreProviders)
	return nil, nil
}
