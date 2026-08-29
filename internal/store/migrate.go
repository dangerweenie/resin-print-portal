package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/dangerweenie/resin-print-portal/db"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
	"github.com/pressly/goose/v3"
)

// Migrate runs embedded goose migrations against dsn. direction is "up",
// "down", or "status"; "up" is what the Helm migrate Job uses. It retries the
// initial connection for up to two minutes so a fresh install can race the
// bundled Postgres coming online.
func Migrate(dsn, direction string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer sqlDB.Close()

	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err = sqlDB.Ping(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("database not reachable after retries: %w", err)
		}
		time.Sleep(3 * time.Second)
	}

	goose.SetBaseFS(db.Migrations)
	goose.SetTableName("goose_db_version")
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("dialect: %w", err)
	}

	switch direction {
	case "up", "":
		return goose.Up(sqlDB, "migrations")
	case "down":
		return goose.Down(sqlDB, "migrations")
	case "status":
		return goose.Status(sqlDB, "migrations")
	default:
		return fmt.Errorf("unknown migrate direction %q (want up|down|status)", direction)
	}
}
