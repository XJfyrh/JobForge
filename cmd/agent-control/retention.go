package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

func cleanupTerminal(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("cleanup-terminal", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	database := flags.String("database", "", "exact target database name")
	limit := flags.Int("limit", 100, "maximum terminal Runs")
	apply := flags.Bool("apply", false, "purge content; default is dry-run")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *database == "" || *limit < 1 || *limit > 100 {
		return errors.New("cleanup-terminal requires --database and --limit 1..100; --apply is explicit")
	}
	config, err := pgxpool.ParseConfig(os.Getenv("JOBFORGE_AGENT_DSN"))
	if err != nil || config.ConnConfig.Database != *database {
		return errors.New("cleanup database does not match the explicit target")
	}
	config.MaxConns = 2
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(bounded, config)
	if err != nil {
		return errors.New("cleanup PostgreSQL unavailable")
	}
	defer pool.Close()
	var allowed bool
	err = pool.QueryRow(bounded, `select current_database()=$1 and
		to_regclass('business_meta.database_identity') is null and
		exists(select 1 from information_schema.columns where table_schema='public'
		and table_name='runs' and column_name='content_purged_at')`, *database).Scan(&allowed)
	if err != nil || !allowed {
		return errors.New("cleanup refused an unavailable or wrong-purpose database")
	}
	store, err := runpostgres.New(pool, runpostgres.Options{})
	if err != nil {
		return errors.New("cleanup store unavailable")
	}
	result, err := store.CleanupTerminalContent(bounded, *limit, *apply)
	if err != nil {
		return errors.New("terminal content cleanup failed; inspect persisted expiry before retry")
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
