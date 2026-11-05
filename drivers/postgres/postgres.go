// Package postgres links the pgx Postgres driver into the host binary so
// modules (db_sql) can open "pgx" connections:
//
//	import _ "kiln/drivers/postgres"
package postgres

import _ "github.com/jackc/pgx/v5/stdlib"
