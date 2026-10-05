package business_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xjfyrh/jobforge/internal/business"
)

type actionGuardBarrierKey struct{}

type actionGuardBarrier struct {
	once    sync.Once
	entered chan uint32
	release chan struct{}
}

func (*actionGuardBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "pg_advisory_xact_lock") {
		return context.WithValue(ctx, actionGuardBarrierKey{}, true)
	}
	return ctx
}

func (b *actionGuardBarrier) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if marked, _ := ctx.Value(actionGuardBarrierKey{}).(bool); !marked || data.Err != nil {
		return
	}
	b.once.Do(func() {
		b.entered <- conn.PgConn().PID()
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	})
}

func TestBusinessActionSerializesActualLoadersAndMissingInsertions(t *testing.T) {
	for _, change := range []string{"missing-order", "missing-delivery", "publish-index"} {
		for _, leading := range []string{"loader", "action"} {
			t.Run(change+"/"+leading+"-first", func(t *testing.T) {
				parent, admin, loaderPool, runtimePool := isolatedPools(t)
				ctx, cancel := context.WithTimeout(parent, 8*time.Second)
				defer cancel()
				dataset, upload := fixture()
				incomplete := dataset
				incomplete.DatasetVersion = "before-missing-fact"
				if change == "publish-index" {
					policy := dataset.Policies[0]
					policy.PolicyVersion = "policy-v2"
					incomplete.Policies = append(append([]business.PolicyVersion(nil), dataset.Policies...), policy)
				}
				if change != "publish-index" {
					incomplete.Deliveries = dataset.Deliveries[1:]
					if change == "missing-order" {
						incomplete.Orders = dataset.Orders[1:]
					}
				}
				seedConsistencyFixture(ctx, t, business.NewStore(loaderPool), incomplete, upload)
				snapshot, _, err := business.NewStore(runtimePool).CreateSnapshot(ctx, "tenant-north", business.SnapshotRequest{SchemaVersion: 1, TicketID: "ticket-1", RequestKey: "before-concurrent-source"})
				if err != nil {
					t.Fatal(err)
				}
				action, keys := authorizeFixture(ctx, t, admin, snapshot)
				barrier := &actionGuardBarrier{entered: make(chan uint32, 1), release: make(chan struct{})}
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
				defer release()
				config := admin.Config().Copy()
				config.ConnConfig.User, config.ConnConfig.Password = "jobforge_business_writer_login", "jobforge_business_writer_login"
				writerPool, err := pgxpool.NewWithConfig(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer writerPool.Close()
				config = writerPool.Config().Copy()
				if leading == "loader" {
					config = loaderPool.Config().Copy()
				}
				config.ConnConfig.Tracer = barrier
				traced, err := pgxpool.NewWithConfig(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer traced.Close()
				writer, loader := business.NewStore(writerPool), business.NewStore(loaderPool)
				if leading == "loader" {
					loader = business.NewStore(traced)
				} else {
					writer = business.NewStore(traced)
				}
				// If the action commits first, the later import describes that
				// actual ticket version, so it can add the formerly absent row.
				if leading == "action" && change != "publish-index" {
					dataset.Tickets[0].Revision = 2
				}
				dataset.DatasetVersion = "concurrent-missing-fact"
				if change == "publish-index" {
					upload.Profile.PolicyVersion = "policy-v2"
				}
				apply := func() error {
					_, err := writer.ApplyResolution(ctx, action.Authorization.TenantID, action, keys)
					return err
				}
				load := func() error {
					if change == "publish-index" {
						_, reused, err := loader.PublishIndex(ctx, upload)
						if err == nil && reused {
							return errors.New("new index unexpectedly reused")
						}
						return err
					}
					return loader.ImportDataset(ctx, dataset)
				}
				first, second := apply, load
				if leading == "loader" {
					first, second = load, apply
				}
				firstDone, secondDone := make(chan error, 1), make(chan error, 1)
				go func() { firstDone <- first() }()
				var pid uint32
				select {
				case pid = <-barrier.entered:
				case <-ctx.Done():
					t.Fatal("leading real transaction did not acquire the guard")
				}
				go func() { secondDone <- second() }()
				for {
					var blocked bool
					if err := admin.QueryRow(ctx, `select exists(select 1 from pg_stat_activity where $1=any(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
						t.Fatal(err)
					}
					if blocked {
						break
					}
					select {
					case <-time.After(10 * time.Millisecond):
					case <-ctx.Done():
						t.Fatal("concurrent transaction bypassed the source guard")
					}
				}
				release()
				if err := <-firstDone; err != nil {
					t.Fatal("leading actual transaction", err)
				}
				secondErr := <-secondDone
				wantConflict := leading == "loader" && change != "publish-index"
				if wantConflict && !errors.Is(secondErr, business.ErrActionConflict) || !wantConflict && secondErr != nil {
					t.Fatal("post-guard snapshot/source validation", secondErr)
				}
				var effects, revision int
				if err := admin.QueryRow(ctx, `select revision,(select count(*) from business.ticket_resolutions)
					from business.tickets where tenant_id='tenant-north' and ticket_id='ticket-1'`).Scan(&revision, &effects); err != nil {
					t.Fatal(err)
				}
				want := 1
				if wantConflict {
					want = 0
				}
				if effects != want || revision != int(1+want) {
					t.Fatal("concurrent effect/version", effects, revision)
				}
			})
		}
	}
}

func TestBusinessActionRejectsChangedIndexBinding(t *testing.T) {
	ctx, _, _, writer, _, action, keys := actionFixture(t)
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	for _, field := range []string{"id", "profile_hash", "content_hash"} {
		changed := action.Authorization
		switch field {
		case "id":
			changed.VersionVector.Index.ID = "00000000-0000-4000-8000-000000000001"
		case "profile_hash":
			changed.VersionVector.Index.ProfileHash = strings.Repeat("e", 64)
		case "content_hash":
			changed.VersionVector.Index.ContentHash = strings.Repeat("e", 64)
		}
		signed, err := business.SignAction(changed, action.Parameters, private)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.ApplyResolution(ctx, signed.Authorization.TenantID, signed, keys); !errors.Is(err, business.ErrActionConflict) {
			t.Fatal("changed signed index binding", field, err)
		}
	}
}
