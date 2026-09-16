// Command support-business runs the isolated Agent v3 business dependency.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	supportdata "github.com/xjfyrh/jobforge/examples/support-agent/runtime"
	"github.com/xjfyrh/jobforge/internal/business"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		// Never print a pgx/config error: it can contain credentials or source facts.
		fmt.Fprintln(os.Stderr, "business command failed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("expected serve, migrate, seed or publish-index")
	}
	flags := flag.NewFlagSet("support-business", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	initialize := flags.Bool("initialize", false, "mark an empty dedicated database before migrations")
	down := flags.Bool("down", false, "roll back only business schemas in a marked database")
	input := flags.String("file", "", "bounded operator JSON input")
	tenant := flags.String("tenant", "", "operator-selected tenant for index publication")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return errors.New("invalid command flags")
	}
	config, err := pgxpool.ParseConfig(os.Getenv("JOBFORGE_BUSINESS_DSN"))
	if err != nil || os.Getenv("JOBFORGE_BUSINESS_DSN") == "" {
		return errors.New("invalid JOBFORGE_BUSINESS_DSN")
	}
	config.MaxConns = 8
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return errors.New("business database unavailable")
	}
	defer pool.Close()
	if args[0] == "serve" {
		if *initialize || *down || *input != "" || *tenant != "" {
			return errors.New("unexpected serve flags")
		}
		return serve(ctx, pool)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	store := business.NewStore(pool)
	switch args[0] {
	case "migrate":
		if *input != "" || *tenant != "" || (*initialize && *down) {
			return errors.New("invalid migration flags")
		}
		migrator := business.NewMigrator(pool)
		if *initialize {
			if err := migrator.Initialize(ctx); err != nil {
				return errors.New("business database initialization refused")
			}
		}
		if *down {
			if err := migrator.Down(ctx); err != nil {
				return errors.New("business rollback failed")
			}
		} else if err := migrator.Up(ctx); err != nil {
			return errors.New("business migration failed")
		}
		fmt.Println(`{"migration":"complete"}`)
	case "seed":
		if *initialize || *down || *tenant != "" {
			return errors.New("invalid seed flags")
		}
		var dataset business.Dataset
		if err := readJSONFile(*input, 1<<20, &dataset); err != nil {
			return err
		}
		if err := store.ImportDataset(ctx, dataset); err != nil {
			return errors.New("business dataset rejected")
		}
		fmt.Println(`{"seed":"complete"}`)
	case "publish-index":
		if *initialize || *down || *tenant == "" {
			return errors.New("invalid index publication flags")
		}
		var upload business.IndexUpload
		if err := readJSONFile(*input, 2<<20, &upload); err != nil {
			return err
		}
		if upload.TenantID != "" && upload.TenantID != *tenant {
			return errors.New("index tenant conflicts with operator selection")
		}
		upload.TenantID = *tenant
		if err := business.ValidateRegisteredIndex(upload, supportdata.Files); err != nil {
			return errors.New("index does not match the registered policy corpus")
		}
		index, _, err := store.PublishIndex(ctx, upload)
		if err != nil {
			return errors.New("business index rejected")
		}
		if err := json.NewEncoder(os.Stdout).Encode(index); err != nil {
			return errors.New("index report unavailable")
		}
	default:
		return errors.New("unknown business command")
	}
	return nil
}

func readJSONFile(path string, limit int64, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return errors.New("operator input unavailable")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("operator input exceeds regular file limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return errors.New("operator input unavailable or oversized")
	}
	if err := decodeJSON(data, value); err != nil {
		return errors.New("operator input is not strict JSON")
	}
	return nil
}

func serve(ctx context.Context, pool *pgxpool.Pool) error {
	store := business.NewStore(pool)
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := store.CheckRuntimeRole(readyCtx)
	if err == nil {
		err = store.CheckReady(readyCtx)
	}
	cancel()
	if err != nil {
		return errors.New("business database not ready")
	}
	var keys map[string]business.Identity
	keyJSON := os.Getenv("JOBFORGE_BUSINESS_KEYS")
	if len(keyJSON) > 65536 || decodeJSON([]byte(keyJSON), &keys) != nil {
		return errors.New("invalid JOBFORGE_BUSINESS_KEYS")
	}
	handler, err := business.NewHTTPHandler(store, keys)
	if err != nil {
		return errors.New("business identities rejected")
	}
	resourceCtx, resourceCancel := context.WithTimeout(ctx, 10*time.Second)
	defer resourceCancel()
	for _, identity := range keys {
		if err := store.CheckTenantReady(resourceCtx, identity.TenantID); err != nil {
			return errors.New("business tenant resources not ready")
		}
	}
	addr := os.Getenv("JOBFORGE_BUSINESS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8092"
	}
	server := &http.Server{
		Addr: addr, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 12 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192,
	}
	stopped := make(chan error, 1)
	go func() { stopped <- server.ListenAndServe() }()
	select {
	case err := <-stopped:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("business HTTP server failed")
		}
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return errors.New("business HTTP shutdown deadline exceeded")
		}
	}
	return nil
}
