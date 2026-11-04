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
//
// Example:
//
//	// import _ "github.com/ganeshdipdumbare/eipagent/drivers/postgres"
//	eipagent.WithPlugin("db_sql", map[string]any{"dsn": "secret://DATABASE_URL", "max_open": 20})
var Permissions = []string{"db"}

func Apply(p *sdk.Plugin) error {
	driver := p.ConfigString("driver", "pgx")
	dsn, err := p.SecretValue(p.ConfigString("dsn", "secret://EIP_DATABASE_URL"))
	if err != nil {
		return err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return fmt.Errorf("db_sql: %w (is the driver imported in the host?)", err)
	}
	db.SetMaxOpenConns(p.ConfigInt("max_open", 10))
	db.SetMaxIdleConns(p.ConfigInt("max_idle", 5))
	db.SetConnMaxLifetime(30 * time.Minute)
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(c); err != nil {
		db.Close()
		return fmt.Errorf("db_sql: ping: %w", err)
	}
	p.OnUnload(func() { db.Close() })
	name := "db"
	if n := p.ConfigString("name", ""); n != "" {
		name = "db/" + n
	}
	return p.Provide(name, db)
}
