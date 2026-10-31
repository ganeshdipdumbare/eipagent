//go:build ignore

package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"eipagent/sdk"
)

// Postgres long-term store over the "db" service, provided as "store".
// Search is a case-insensitive substring match over keys and values.
//
// Config: table (default "eip_store").
var Inject = []string{"db"}

var Permissions = []string{"db"}

const sep = "\x1f"

func Apply(ctx *sdk.Context) error {
	db := ctx.Service("db").(*sql.DB)
	table := ctx.ConfigString("table", "eip_store")
	for _, r := range table {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return errors.New("store_sql: invalid table name")
		}
	}
	c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := db.ExecContext(c, `CREATE TABLE IF NOT EXISTS `+table+` (
		ns TEXT NOT NULL,
		key TEXT NOT NULL,
		value JSONB NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (ns, key))`); err != nil {
		return err
	}
	nsKey := func(ns []string) string { return strings.Join(ns, sep) + sep }
	scan := func(rows *sql.Rows) ([]sdk.StoreItem, error) {
		defer rows.Close()
		var out []sdk.StoreItem
		for rows.Next() {
			var ns, key string
			var raw []byte
			var ts time.Time
			if err := rows.Scan(&ns, &key, &raw, &ts); err != nil {
				return nil, err
			}
			it := sdk.StoreItem{Namespace: strings.Split(strings.TrimSuffix(ns, sep), sep), Key: key, Updated: ts.UnixMilli()}
			json.Unmarshal(raw, &it.Value)
			out = append(out, it)
		}
		return out, rows.Err()
	}
	return ctx.Provide("store", &sdk.Store{
		Put: func(c context.Context, ns []string, key string, val map[string]any) error {
			raw, err := json.Marshal(val)
			if err != nil {
				return err
			}
			_, err = db.ExecContext(c, `INSERT INTO `+table+` (ns, key, value) VALUES ($1, $2, $3)
				ON CONFLICT (ns, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, nsKey(ns), key, raw)
			return err
		},
		Get: func(c context.Context, ns []string, key string) (*sdk.StoreItem, error) {
			rows, err := db.QueryContext(c, `SELECT ns, key, value, updated_at FROM `+table+` WHERE ns = $1 AND key = $2`, nsKey(ns), key)
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
			rows, err := db.QueryContext(c, `SELECT ns, key, value, updated_at FROM `+table+`
				WHERE starts_with(ns, $1) AND ($2 = '' OR key ILIKE '%' || $2 || '%' OR value::text ILIKE '%' || $2 || '%')
				ORDER BY updated_at DESC LIMIT $3`, nsKey(ns), q, limit)
			if err != nil {
				return nil, err
			}
			return scan(rows)
		},
		Delete: func(c context.Context, ns []string, key string) error {
			_, err := db.ExecContext(c, `DELETE FROM `+table+` WHERE ns = $1 AND key = $2`, nsKey(ns), key)
			return err
		},
	})
}
