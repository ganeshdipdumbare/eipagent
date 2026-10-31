//go:build ignore

package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"eipagent/sdk"
)

// Postgres thread checkpointer over the "db" service, provided as
// "checkpointer". Optimistic versioning lets many stateless replicas
// share threads safely. The table is created on first load.
//
// Config: table (default "eip_threads").
var Inject = []string{"db"}

var Permissions = []string{"db"}

func Apply(ctx *sdk.Context) error {
	db := ctx.Service("db").(*sql.DB)
	table := ctx.ConfigString("table", "eip_threads")
	if !validIdent(table) {
		return errors.New("checkpoint_sql: invalid table name")
	}
	c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := db.ExecContext(c, `CREATE TABLE IF NOT EXISTS `+table+` (
		id TEXT PRIMARY KEY,
		version BIGINT NOT NULL,
		state JSONB NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	return ctx.Provide("checkpointer", &sdk.Checkpointer{
		Load: func(c context.Context, id string) (*sdk.ThreadState, error) {
			var raw []byte
			err := db.QueryRowContext(c, `SELECT state FROM `+table+` WHERE id = $1`, id).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			var st sdk.ThreadState
			if err := json.Unmarshal(raw, &st); err != nil {
				return nil, err
			}
			return &st, nil
		},
		Save: func(c context.Context, st *sdk.ThreadState, expected int64) error {
			st.Version = expected + 1
			raw, err := json.Marshal(st)
			if err != nil {
				return err
			}
			var res sql.Result
			if expected == 0 {
				res, err = db.ExecContext(c, `INSERT INTO `+table+` (id, version, state) VALUES ($1, 1, $2) ON CONFLICT (id) DO NOTHING`, st.ID, raw)
			} else {
				res, err = db.ExecContext(c, `UPDATE `+table+` SET version = $3, state = $2, updated_at = now() WHERE id = $1 AND version = $4`, st.ID, raw, st.Version, expected)
			}
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				st.Version = expected
				return sdk.ErrVersionConflict
			}
			return nil
		},
		Delete: func(c context.Context, id string) error {
			_, err := db.ExecContext(c, `DELETE FROM `+table+` WHERE id = $1`, id)
			return err
		},
		List: func(c context.Context, prefix string) ([]string, error) {
			rows, err := db.QueryContext(c, `SELECT id FROM `+table+` WHERE starts_with(id, $1) ORDER BY id`, prefix)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return nil, err
				}
				out = append(out, id)
			}
			return out, rows.Err()
		},
	})
}

func validIdent(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
