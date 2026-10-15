// Command runner executes only a frozen S5 diagnosis, outside production images.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	diagnosis "github.com/xjfyrh/jobforge/tools/support_diagnosis"
)

func main() {
	manifest := flag.String("manifest", "", "immutable diagnosis manifest")
	credentials := flag.String("credentials", "", "existing private worker credential file")
	initialize := flag.Bool("init", false, "initialize only an isolated diagnosis database")
	verify := flag.Bool("check", false, "verify manifest and runner without database access or model calls")
	deadline := flag.String("deadline", "", "reviewed UTC authorization deadline for a contract probe")
	flag.Parse()
	if *verify {
		m, _, err := diagnosis.LoadManifest(*manifest)
		if err != nil || diagnosis.VerifyBinary(m.RunnerSHA256) != nil {
			fmt.Fprintln(os.Stderr, "diagnosis manifest verification failed")
			os.Exit(1)
		}
		_, _ = fmt.Fprintln(os.Stdout, "diagnosis manifest and runner verified; no model calls")
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if *deadline != "" {
		until, err := time.Parse(time.RFC3339Nano, *deadline)
		if err != nil || !until.After(time.Now()) {
			fmt.Fprintln(os.Stderr, "diagnosis authorization deadline invalid")
			os.Exit(1)
		}
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithDeadline(ctx, until)
		defer deadlineCancel()
	}
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
