// Command runner executes only a frozen S5 diagnosis, outside production images.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/jackc/pgx/v5/pgxpool"
	diagnosis "github.com/xjfyrh/jobforge/tools/support_diagnosis"
)

func main() {
	manifest := flag.String("manifest", "", "immutable diagnosis manifest")
	credentials := flag.String("credentials", "", "existing private worker credential file")
	initialize := flag.Bool("init", false, "initialize only an isolated diagnosis database")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("JOBFORGE_DIAGNOSIS_DSN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "diagnosis database unavailable")
		os.Exit(1)
	}
	defer pool.Close()
	if *initialize {
		err = diagnosis.Initialize(ctx, pool)
	} else {
		err = diagnosis.Execute(ctx, pool, *manifest, *credentials, os.Stdout)
	}
	if err != nil {
		// SQL/network errors can contain connection details; never print them.
		fmt.Fprintln(os.Stderr, "diagnosis stopped; inspect the PG ledger and bounded reports")
		os.Exit(1)
	}
}
