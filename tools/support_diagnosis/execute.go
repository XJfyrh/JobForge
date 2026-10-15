package diagnosis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func credential(path string) (string, error) {
	raw, err := os.ReadFile(path)
	var envelope struct {
		Tenants map[string]struct {
			Key           string `json:"deepseek_api_key"`
			BusinessToken string `json:"business_token"`
		} `json:"tenants"`
	}
	// Existing worker credentials may contain unrelated registered credentials.
	if err != nil || json.Unmarshal(raw, &envelope) != nil {
		return "", ErrInvalid
	}
	north, south := envelope.Tenants["tenant-north"].Key, envelope.Tenants["tenant-south"].Key
	if len(north) < 16 || north != south {
		return "", ErrInvalid
	}
	return north, nil
}

// Execute sends each frozen trial at most once, with PG authorization before HTTP.
func Execute(ctx context.Context, pool *pgxpool.Pool, manifestPath, credentialsPath string, progress io.Writer) error {
	m, raw, err := LoadManifest(manifestPath)
	if err != nil || m.SchemaVersion < 2 || m.SchemaVersion > 5 {
		return ErrInvalid
	}
	if m.SchemaVersion >= 4 {
		deadline, set := ctx.Deadline()
		if !set || !deadline.After(time.Now()) {
			return ErrInvalid
		}
	}
	if err := VerifyBinary(m.RunnerSHA256); err != nil {
		return err
	}
	key, err := credential(credentialsPath)
	if err != nil {
		return err
	}
	l, err := OpenLedger(ctx, pool, m, raw)
	if err != nil {
		return err
	}
	defer l.Close()
	root := filepath.Dir(manifestPath)
	if err := os.MkdirAll(filepath.Join(root, "responses"), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "reports"), 0700); err != nil {
		return err
	}
	defer func() {
		end, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if summary, err := l.Summary(end); err == nil {
			_ = json.NewEncoder(progress).Encode(summary)
			_ = writeNew(filepath.Join(root, fmt.Sprintf("ledger-%d.json", time.Now().UnixNano())), summary)
		}
		if snapshot, err := l.Export(end); err == nil {
			_ = writeNew(filepath.Join(root, fmt.Sprintf("ledger-export-%d.json", time.Now().UnixNano())), snapshot)
		}
	}()
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	timeout := 180 * time.Second
	if m.SchemaVersion >= 3 {
		timeout = 120 * time.Second
	}
	client := &http.Client{Transport: transport, Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("redirect rejected") }}
	var health protocolHealth
	for index, trial := range m.Trials {
		if ctx.Err() != nil {
			return errors.New("diagnosis cancelled")
		}
		settled, err := l.Settled(ctx, trial.ID)
		if err != nil {
			return err
		}
		if settled {
			continue
		}
		body, err := requestBytes(root, trial)
		if err != nil {
			return err
		}
		batch, err := l.NextBatch(ctx)
		if err != nil {
			return err
		}
		if err := l.Reserve(ctx, batch, trial, index+1); err != nil {
			return err
		}
		start := time.Now()
		report, response := send(ctx, client, body, key, trial)
		elapsed := time.Since(start).Milliseconds()
		if m.SchemaVersion >= 4 && report.Outcome == "final" && !probeFinalShape(body, response) {
			report.Outcome = "invalid_final"
		}
		if m.SchemaVersion >= 4 && report.Stop == "" && report.Usage != nil &&
			health.stop(index+1, report.Outcome) {
			report.Stop = "systemic_protocol_failure"
		}
		// Preserve billing even if local raw-body persistence fails after reception.
		if err := writeRawNew(filepath.Join(root, "responses", trial.ID+".json"), response); err != nil {
			report.Stop = "evidence_write_failed"
		}
		end, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err = l.Settle(end, trial, report, elapsed)
		cancel()
		if err != nil {
			return errors.New("diagnosis settlement unresolved; no further sends")
		}
		if err := writeNew(filepath.Join(root, "reports", trial.ID+".json"), report); err != nil {
			return err
		}
		if err := json.NewEncoder(progress).Encode(map[string]any{"trial": trial.ID, "ordinal": index + 1,
			"outcome": report.Outcome, "elapsed_ms": elapsed, "usage_known": report.Usage != nil, "stop": report.Stop}); err != nil {
			return err
		}
		if report.Stop != "" || report.Usage == nil {
			return errors.New("diagnosis stopped by provider or usage audit")
		}
	}
	return nil
}

type protocolHealth struct{ consecutive, firstThree int }

func (h *protocolHealth) stop(ordinal int, outcome string) bool {
	if outcome == "invalid_final" || outcome == "incomplete_output" {
		h.consecutive++
		if ordinal <= 3 {
			h.firstThree++
		}
	} else {
		h.consecutive = 0
	}
	return h.consecutive >= 2 || h.firstThree >= 2
}

func send(ctx context.Context, client *http.Client, body []byte, key string, trial Trial) (Report, []byte) {
	r := Report{ResponseSHA256: Digest(nil), Outcome: "transport_unknown", Stop: "transport_unknown"}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.deepseek.com/chat/completions", bytes.NewReader(body))
	if err != nil {
		return r, nil
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return r, nil
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		r.ResponseSHA256 = Digest(raw)
		return r, raw
	}
	group := trial.Group
	if finalOnlyGroup(group) {
		group = "B"
	}
	return ParseReport(raw, response.StatusCode, group, trial.ID), raw
}
