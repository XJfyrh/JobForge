package business

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	businessmigrations "github.com/xjfyrh/jobforge/migrations/business"
)

const businessMigrationLock int64 = 7483921655

// Migrator is exclusively for a purpose-marked, dedicated business database.
// It is never invoked by the ordinary control-plane migrator.
type Migrator struct{ pool *pgxpool.Pool }

// NewMigrator uses an explicitly supplied migration/admin pool.
func NewMigrator(pool *pgxpool.Pool) *Migrator { return &Migrator{pool: pool} }

// Initialize explicitly marks a fresh empty database. A core or other populated
// database is rejected before any DDL or global role change is attempted.
func (m *Migrator) Initialize(ctx context.Context) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return ErrDependencyUnavailable
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, businessMigrationLock); err != nil {
		return ErrDependencyUnavailable
	}
	var marked bool
	if err := tx.QueryRow(ctx, `select to_regclass('business_meta.database_identity') is not null`).Scan(&marked); err != nil {
		return ErrWrongDatabase
	}
	if marked {
		if err := validatePurpose(ctx, tx); err != nil {
			return err
		}
		return publicCommit(ctx, tx)
	}
	var empty bool
	err = tx.QueryRow(ctx, `select not exists (
 select 1 from pg_catalog.pg_class c join pg_catalog.pg_namespace n on n.oid = c.relnamespace
 where n.nspname !~ '^pg_' and n.nspname <> 'information_schema'
 ) and not exists (
 select 1 from pg_catalog.pg_namespace where nspname !~ '^pg_'
 and nspname not in ('public', 'information_schema')
 ) and not exists (
 select 1 from pg_catalog.pg_proc p join pg_catalog.pg_namespace n on n.oid = p.pronamespace
 where n.nspname = 'public'
 ) and not exists (select 1 from pg_catalog.pg_extension where extname <> 'plpgsql')`).Scan(&empty)
	if err != nil || !empty {
		return ErrWrongDatabase
	}
	if err := applyBusinessSQL(ctx, tx, "0001_business_identity.up.sql"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `insert into business_meta.business_schema_migrations (version) values (1)`); err != nil {
		return ErrInternal
	}
	return publicCommit(ctx, tx)
}

// Up applies only business migrations after verifying the existing marker.
func (m *Migrator) Up(ctx context.Context) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return ErrDependencyUnavailable
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, businessMigrationLock); err != nil {
		return ErrDependencyUnavailable
	}
	if err := validatePurpose(ctx, tx); err != nil {
		return err
	}
	var version int
	if err := tx.QueryRow(ctx, `select coalesce(max(version), 0) from business_meta.business_schema_migrations`).Scan(&version); err != nil || version < 1 || version > 2 {
		return ErrWrongDatabase
	}
	if version == 1 {
		if err := applyBusinessSQL(ctx, tx, "0002_business_facts.up.sql"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into business_meta.business_schema_migrations (version) values (2)`); err != nil {
			return ErrInternal
		}
	}
	var extensionOK bool
	err = tx.QueryRow(ctx, `select exists (select 1 from pg_catalog.pg_extension e
 join pg_catalog.pg_namespace n on n.oid = e.extnamespace
 where e.extname = 'vector' and e.extversion = '0.8.6' and n.nspname = 'extensions')`).Scan(&extensionOK)
	if err != nil || !extensionOK {
		return ErrDependencyUnavailable
	}
	return publicCommit(ctx, tx)
}

// Down rolls back facts and vectors in an explicitly disposable business DB.
// The identity marker and global restricted roles are intentionally retained.
func (m *Migrator) Down(ctx context.Context) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return ErrDependencyUnavailable
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, businessMigrationLock); err != nil {
		return ErrDependencyUnavailable
	}
	if err := validatePurpose(ctx, tx); err != nil {
		return err
	}
	var version int
	if err := tx.QueryRow(ctx, `select coalesce(max(version), 0) from business_meta.business_schema_migrations`).Scan(&version); err != nil || version < 1 || version > 2 {
		return ErrWrongDatabase
	}
	if version == 2 {
		if err := applyBusinessSQL(ctx, tx, "0002_business_facts.down.sql"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `delete from business_meta.business_schema_migrations where version = 2`); err != nil {
			return ErrInternal
		}
	}
	return publicCommit(ctx, tx)
}

func validatePurpose(ctx context.Context, tx pgx.Tx) error {
	var valid bool
	err := tx.QueryRow(ctx, `select purpose = 'jobforge-business-v1'
 and database_name = current_database()
 and to_regclass('public.schema_migrations') is null and to_regclass('public.jobs') is null
 from business_meta.database_identity where singleton`).Scan(&valid)
	if err != nil || !valid {
		return ErrWrongDatabase
	}
	return nil
}

func applyBusinessSQL(ctx context.Context, tx pgx.Tx, name string) error {
	script, err := businessmigrations.Files.ReadFile(name)
	if err != nil {
		return ErrInternal
	}
	if _, err := tx.Exec(ctx, string(script)); err != nil {
		return ErrInternal
	}
	return nil
}
