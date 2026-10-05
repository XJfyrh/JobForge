package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/business"
)

func actionHandler(ctx context.Context, ordinary map[string]business.Identity) (http.Handler, func(), error) {
	writerDSN := os.Getenv("JOBFORGE_BUSINESS_ACTION_DSN")
	readerDSN := os.Getenv("JOBFORGE_BUSINESS_RECEIPT_DSN")
	identitiesJSON := os.Getenv("JOBFORGE_BUSINESS_ACTION_KEYS")
	publicJSON := os.Getenv("JOBFORGE_BUSINESS_ACTION_PUBLIC_KEYS")
	noop := func() {}
	if writerDSN == "" && readerDSN == "" && identitiesJSON == "" && publicJSON == "" {
		return nil, noop, nil
	}
	if writerDSN == "" || readerDSN == "" || len(identitiesJSON) == 0 || len(identitiesJSON) > 65536 || len(publicJSON) == 0 || len(publicJSON) > 16384 {
		return nil, noop, errors.New("incomplete business action configuration")
	}
	var identities map[string]business.Identity
	var keyConfig map[string]business.ActionKeyConfig
	if decodeJSON([]byte(identitiesJSON), &identities) != nil || decodeJSON([]byte(publicJSON), &keyConfig) != nil {
		return nil, noop, errors.New("invalid business action configuration")
	}
	for token := range identities {
		if _, exists := ordinary[token]; exists {
			return nil, noop, errors.New("overlapping business action credentials")
		}
	}
	publicKeys, err := business.DecodeActionKeys(keyConfig)
	if err != nil {
		return nil, noop, errors.New("invalid business action public keys")
	}
	pools := make([]*pgxpool.Pool, 0, 2)
	closePools := func() {
		for _, pool := range pools {
			pool.Close()
		}
	}
	for _, dsn := range []string{writerDSN, readerDSN} {
		config, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			closePools()
			return nil, noop, errors.New("invalid business action database configuration")
		}
		config.MaxConns, config.ConnConfig.ConnectTimeout = 4, 5*time.Second
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			closePools()
			return nil, noop, errors.New("business action database unavailable")
		}
		pools = append(pools, pool)
	}
	writer, reader := business.NewStore(pools[0]), business.NewStore(pools[1])
	ready, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if writer.CheckActionRole(ready, true) != nil || reader.CheckActionRole(ready, false) != nil {
		closePools()
		return nil, noop, errors.New("business action database privileges rejected")
	}
	handler, err := business.NewActionHTTPHandler(writer, reader, identities, publicKeys)
	if err != nil {
		closePools()
		return nil, noop, errors.New("business action identities rejected")
	}
	return handler, closePools, nil
}
