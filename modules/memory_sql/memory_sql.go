package memory_sql

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Postgres long-term memory over the "db" service, provided as "memory".
// Search is a case-insensitive substring match over keys and values.
//
// Config: table (default "eip_store").
//
// Example:
//
//	kiln.WithModuleConfig("db_sql", map[string]any{"dsn": "secret://DATABASE_URL"})
//	kiln.WithModuleConfig("memory_sql", map[string]any{"table": "agent_memory"})
//	// Shortcut with threads_sql too: kiln.WithPersistence("postgres", "secret://DATABASE_URL")
var Requires = []string{"db"}

var Permissions = []string{"db"}

const sep = "\x1f"

func Apply(p *sdk.Module) error {
	db, err := p.DB()
	if err != nil {
		return err
	}
	table := p.ConfigString("table", "eip_store")
	for _, r := range table {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return errors.New("memory_sql: invalid table name")
		}
	}
	c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := db.Exec(c, `CREATE TABLE IF NOT EXISTS `+table+` (
		ns TEXT NOT NULL,
		key TEXT NOT NULL,
		value JSONB NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (ns, key))`); err != nil {
		return err
	}
	nsKey := func(ns []string) string { return strings.Join(ns, sep) + sep }
	scan := func(rows []map[string]any) ([]sdk.StoreItem, error) {
		var out []sdk.StoreItem
		for _, r := range rows {
			ns, ok1 := asString(r["ns"])
			key, ok2 := asString(r["key"])
			raw, ok3 := asBytes(r["value"])
			ts, ok4 := r["updated_at"].(time.Time)
			if !ok1 || !ok2 || !ok3 || !ok4 {
				return nil, errors.New("memory_sql: unexpected column types")
			}
			it := sdk.StoreItem{Namespace: strings.Split(strings.TrimSuffix(ns, sep), sep), Key: key, Updated: ts.UnixMilli()}
			json.Unmarshal(raw, &it.Value)
			out = append(out, it)
		}
		return out, nil
	}
	return p.Provide("memory", &sdk.Store{
		Put: func(c context.Context, ns []string, key string, val map[string]any) error {
			raw, err := json.Marshal(sdk.RedactMap(val))
			if err != nil {
				return err
			}
			_, err = db.Exec(c, `INSERT INTO `+table+` (ns, key, value) VALUES ($1, $2, $3)
				ON CONFLICT (ns, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, nsKey(ns), key, raw)
			return err
		},
		Get: func(c context.Context, ns []string, key string) (*sdk.StoreItem, error) {
			rows, err := db.Query(c, `SELECT ns, key, value, updated_at FROM `+table+` WHERE ns = $1 AND key = $2`, nsKey(ns), key)
			if err != nil {
				return nil, err
			}
			items, err := scan(rows)
			if err != nil || len(items) == 0 {
				return nil, err
			}
			return &items[0], nil
		},
		Search: func(c context.Context, ns []string, q string, limit int) ([]sdk.StoreItem, error) {
			if limit <= 0 {
				limit = 50
			}
			rows, err := db.Query(c, `SELECT ns, key, value, updated_at FROM `+table+`
				WHERE starts_with(ns, $1) AND ($2 = '' OR key ILIKE '%' || $2 || '%' OR value::text ILIKE '%' || $2 || '%')
				ORDER BY updated_at DESC LIMIT $3`, nsKey(ns), q, limit)
			if err != nil {
				return nil, err
			}
			return scan(rows)
		},
		Delete: func(c context.Context, ns []string, key string) error {
			_, err := db.Exec(c, `DELETE FROM `+table+` WHERE ns = $1 AND key = $2`, nsKey(ns), key)
			return err
		},
	})
}

// asString and asBytes accept what a driver returns for text and JSONB
// columns (string or []byte).
func asString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []byte:
		return string(x), true
	}
	return "", false
}

func asBytes(v any) ([]byte, bool) {
	switch x := v.(type) {
	case []byte:
		return x, true
	case string:
		return []byte(x), true
	}
	return nil, false
}

// Module is the compiled form of "memory_sql". modules/std registers it.
var Module = engine.Module{Name: "memory_sql", Requires: Requires, Provides: []string{"memory"}, Permissions: Permissions, Apply: Apply}
