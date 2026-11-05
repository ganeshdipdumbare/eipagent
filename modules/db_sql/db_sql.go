package db_sql

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// A shared database/sql pool, provided as "db" (or "db/<name>") as an
// *sdk.DB. Other modules use it through p.DB() and never import
// database/sql themselves. This is the only module that does. The driver
// must be linked into the host binary, e.g.
//
//	import _ "kiln/drivers/postgres" // registers "pgx"
//
// Config: driver (default "pgx"), dsn (literal or secret://NAME),
// name (service suffix), max_open (10), max_idle (5).
//
// Example:
//
//	// import _ "github.com/ganeshdipdumbare/kiln/drivers/postgres"
//	kiln.WithModuleConfig("db_sql", map[string]any{"dsn": "secret://DATABASE_URL", "max_open": 20})
var Permissions = []string{"db"}

func Apply(p *sdk.Module) error {
	driver := p.ConfigString("driver", "pgx")
	dsn, err := p.SecretValue(p.ConfigString("dsn", "secret://KILN_DATABASE_URL"))
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
	return p.Provide(name, sdk.NewDB(db))
}

// Module is the compiled form of "db_sql". modules/std registers it.
var Module = engine.Module{Name: "db_sql", Requires: nil, Provides: []string{"db", "db/*"}, Permissions: Permissions, Secrets: []string{"KILN_DATABASE_URL"}, Apply: Apply}
