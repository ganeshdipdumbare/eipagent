package threads_sql

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ganeshdipdumbare/kiln/internal/sqlval"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Postgres thread storage over the "db" service, provided as
// "threads". Optimistic versioning lets many stateless replicas
// share threads safely. The table is created on first load.
//
// Config: table (default "eip_threads"), list_limit (default 10000).
//
// Example:
//
//	db_sql.New(map[string]any{"dsn": "secret://DATABASE_URL"})
//	threads_sql.New(map[string]any{"table": "agent_threads"})
//	// Shortcut with memory_sql too: kiln.Persistence("postgres", "secret://DATABASE_URL")
var Requires = []string{"db"}

var Permissions = []string{"db"}

func Apply(p *sdk.Scope) error {
	db, err := p.DB()
	if err != nil {
		return err
	}
	table := p.ConfigString("table", "eip_threads")
	if !sqlval.ValidIdent(table) {
		return errors.New("threads_sql: invalid table name")
	}
	listLimit := p.ConfigInt("list_limit", 10000)
	if listLimit <= 0 {
		listLimit = 10000
	}
	c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := db.Exec(c, `CREATE TABLE IF NOT EXISTS `+table+` (
		id TEXT PRIMARY KEY,
		version BIGINT NOT NULL,
		state JSONB NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	return p.Provide("threads", &sdk.Checkpointer{
		Load: func(c context.Context, id string) (*sdk.ThreadState, error) {
			rows, err := db.Query(c, `SELECT state FROM `+table+` WHERE id = $1`, id)
			if err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				return nil, nil
			}
			raw, ok := sqlval.AsBytes(rows[0]["state"])
			if !ok {
				return nil, errors.New("threads_sql: state is not JSON bytes")
			}
			var st sdk.ThreadState
			if err := json.Unmarshal(raw, &st); err != nil {
				return nil, err
			}
			return &st, nil
		},
		Save: func(c context.Context, st *sdk.ThreadState, expected int64) error {
			st.Version = expected + 1
			raw, err := json.Marshal(sdk.RedactThreadState(st))
			if err != nil {
				return err
			}
			var n int64
			if expected == 0 {
				n, err = db.Exec(c, `INSERT INTO `+table+` (id, version, state) VALUES ($1, 1, $2) ON CONFLICT (id) DO NOTHING`, st.ID, raw)
			} else {
				n, err = db.Exec(c, `UPDATE `+table+` SET version = $3, state = $2, updated_at = now() WHERE id = $1 AND version = $4`, st.ID, raw, st.Version, expected)
			}
			if err != nil {
				return err
			}
			if n == 0 {
				st.Version = expected
				return sdk.ErrVersionConflict
			}
			return nil
		},
		Delete: func(c context.Context, id string) error {
			_, err := db.Exec(c, `DELETE FROM `+table+` WHERE id = $1`, id)
			return err
		},
		List: func(c context.Context, prefix string) ([]string, error) {
			rows, err := db.Query(c, `SELECT id FROM `+table+` WHERE starts_with(id, $1) ORDER BY id LIMIT $2`, prefix, listLimit)
			if err != nil {
				return nil, err
			}
			var out []string
			for _, r := range rows {
				id, ok := sqlval.AsString(r["id"])
				if !ok {
					return nil, errors.New("threads_sql: id is not text")
				}
				out = append(out, id)
			}
			return out, nil
		},
	})
}

// Module is the compiled form of "threads_sql". modules/std registers it.
var Module = engine.Module{Name: "threads_sql", Requires: Requires, Provides: []string{"threads"}, Permissions: Permissions, Apply: Apply}

// New returns the module. Keys in cfg are the module's configuration; see
// the Config* reads in Apply.
func New(cfg ...map[string]any) engine.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}
