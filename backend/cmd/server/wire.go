//go:build wireinject

package main

import (
	"database/sql"
	"github.com/google/wire"
	"sub2api-enhance/internal/config"
)

func initializeCore(db *sql.DB, cfg *config.Config) (*Core, error) {
	wire.Build(coreProviders)
	return nil, nil
}
