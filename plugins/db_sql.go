//go:build ignore

package plugin

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"eipagent/sdk"
)

// A shared database/sql pool, provided as "db" (or "db/<name>"). The
// driver must be linked into the host binary, e.g.
//
//	import _ "eipagent/drivers/postgres" // registers "pgx"
//
// Config: driver (default "pgx"), dsn (literal or secret://NAME),
// name (service suffix), max_open (10), max_idle (5).
var Permissions = []string{"db"}

func Apply(ctx *sdk.Context) error {
	driver := ctx.ConfigString("driver", "pgx")
	dsn, err := ctx.Secret(ctx.ConfigString("dsn", "secret://EIP_DATABASE_URL"))
	if err != nil {
		return err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return fmt.Errorf("db_sql: %w (is the driver imported in the host?)", err)
	}
	db.SetMaxOpenConns(ctx.ConfigInt("max_open", 10))
	db.SetMaxIdleConns(ctx.ConfigInt("max_idle", 5))
	db.SetConnMaxLifetime(30 * time.Minute)
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(c); err != nil {
		db.Close()
		return fmt.Errorf("db_sql: ping: %w", err)
	}
	ctx.OnDispose(func() { db.Close() })
	name := "db"
	if n := ctx.ConfigString("name", ""); n != "" {
		name = "db/" + n
	}
	return ctx.Provide(name, db)
}
