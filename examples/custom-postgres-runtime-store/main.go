// custom-postgres-runtime-store sketches the boundary between a cloud control
// plane and Lebro's capability-based RuntimeStore. The application owns its
// PostgreSQL schema, migrations, and SaaS models; the adapter maps only the
// neutral Lebro persistence records into that schema.
package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/tesh254/lebro"
)

const backendSchema = "control_plane"

// backendMigrations belong to the application. Tables are explicitly schema
// qualified, unlike Lebro's built-in PostgresStore whose optional Schema is a
// convenience for applications that want Lebro-managed tables.
var backendMigrations = []string{
	`CREATE SCHEMA IF NOT EXISTS control_plane`,
	`CREATE TABLE IF NOT EXISTS control_plane.agent_threads (id text primary key, organization_id text not null, user_id text not null, payload jsonb not null)`,
	`CREATE TABLE IF NOT EXISTS control_plane.workflow_snapshots (run_id text not null, sequence bigint not null, payload jsonb not null, primary key (run_id, sequence))`,
}

func migrateBackend(ctx context.Context, db *sql.DB) error {
	for _, migration := range backendMigrations {
		if _, err := db.ExecContext(ctx, migration); err != nil {
			return err
		}
	}
	return nil
}

// organizationScope is derived by authenticated application middleware, never
// supplied unchecked by an API request. The RuntimeStore adapter uses it for
// transcript, working-memory, workflow snapshot, schedule, and observability
// repository queries.
func organizationScope(organizationID, userID string) lebro.RuntimeScope {
	return lebro.RuntimeScope{Namespace: organizationID, OwnerID: userID}
}

// In a real adapter, each accessor returns a repository backed by one
// transaction-scoped *sql.Tx. Keeping the transaction boundary in the adapter
// lets one agent transcript/working-memory update or one workflow snapshot
// commit atomically. The adapter's Capabilities declares only what it provides.
type postgresRuntimeStore struct{}

func (postgresRuntimeStore) Capabilities() lebro.StoreCapabilities {
	return lebro.StoreCapabilities{Transcript: true, WorkingMemory: true, WorkflowState: true, Transactions: true}
}

func main() {
	scope := organizationScope("org_123", "user_456")
	ctx := lebro.WithRuntimeScope(context.Background(), scope)
	fmt.Printf("backend schema=%s namespace=%s owner=%s\n", backendSchema, scope.Namespace, scope.OwnerID)
	_ = ctx // pass this context to the adapter's transaction-scoped repositories.
}
