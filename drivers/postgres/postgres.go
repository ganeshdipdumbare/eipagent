// Package postgres links the pgx Postgres driver into the host binary so
// interpreted plugins (db_sql) can open "pgx" connections:
//
//	import _ "eipagent/drivers/postgres"
package postgres

import _ "github.com/jackc/pgx/v5/stdlib"
