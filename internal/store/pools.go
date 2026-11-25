package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/proxypool"
)

// Proxy pools and the leases the gateway holds from them.
//
// A pool is one configured IPv6 proxy service: where its management API lives, how
// it is authorized, and where its SOCKS5 service is reached. An upstream names a
// pool, and every account of that upstream is given its own lease from it — a
// dedicated exit address, which is what keeps two accounts of one upstream from
// looking like one client to it.
//
// Both tables are gateway-owned: the upstream schema has nowhere to keep them, and
// the console edits the pools as one more resource.

const gatewayPoolDDL = `CREATE TABLE IF NOT EXISTS gateway_proxy_pools (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	admin_url TEXT NOT NULL,
	token TEXT NOT NULL DEFAULT '',
	socks_host TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL DEFAULT 'auto',
	enabled INTEGER NOT NULL DEFAULT 1
)`

const gatewayPoolAssignmentDDL = `CREATE TABLE IF NOT EXISTS gateway_pool_assignments (
	pool_id INTEGER NOT NULL,
	owner_key TEXT NOT NULL,
	lease_id TEXT NOT NULL,
	ipv6 TEXT NOT NULL DEFAULT '',
	port INTEGER NOT NULL DEFAULT 0,
	site_id INTEGER NOT NULL DEFAULT 0,
	socks_url TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL,
	PRIMARY KEY (pool_id, owner_key)
)`

// poolResource is the console's view of one pool.
var poolResource = Resource{
	Name: "pools", Table: "gateway_proxy_pools", OrderBy: "id",
	Columns: []Column{
		{Name: "name", Kind: KindText, Required: true, MaxLength: 200},
		{Name: "admin_url", Kind: KindText, Required: true, MaxLength: 2048, Validate: validateHTTPURL},
		// The token is a credential: it is masked in every listing and left alone by
		// an update that sends the mask back.
		{Name: "token", Kind: KindText, Secret: true, Clearable: true, MaxLength: 4096},
		{Name: "socks_host", Kind: KindText, MaxLength: 255, Validate: validateHost},
		// auto decides from what the pool reports; the other two name the layout.
		{Name: "mode", Kind: KindText, MaxLength: 16, Choices: []string{"auto", "per_ipv6", "multiplex"}, DefaultValue: "auto"},
		{Name: "enabled", Kind: KindBool, Default: true},
	},
	CascadeDelete: func(ctx context.Context, tx *sql.Tx, id int64) (map[string]int64, error) {
		removed, err := deletePoolAssignmentsForPool(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if removed > 0 {
			return map[string]int64{"pool_assignments": removed}, nil
		}
		return map[string]int64{}, nil
	},
}

// validateHost accepts a host name or address without a scheme or a port, which is
// what the SOCKS5 override is.
func validateHost(value any) error {
	text, ok := value.(string)
	if !ok {
		return invalidField("socks_host", "a host name is required")
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if strings.Contains(trimmed, "://") {
		return invalidField("socks_host", "use a bare host, not a URL")
	}
	if strings.Contains(trimmed, "/") {
		return invalidField("socks_host", "use a host, not a path")
	}
	return nil
}

// EnsurePoolSchema creates the pool tables on a database that does not have them
// yet.
func (s *SQLiteStore) EnsurePoolSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, gatewayPoolDDL); err != nil {
		return fmt.Errorf("create gateway_proxy_pools: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, gatewayPoolAssignmentDDL); err != nil {
		return fmt.Errorf("create gateway_pool_assignments: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS gateway_pool_assignments_site_idx ON gateway_pool_assignments(site_id)`); err != nil {
		return fmt.Errorf("index gateway_pool_assignments: %w", err)
	}
	return nil
}

// loadPools reads every configured pool into the configuration snapshot.
func (s *SQLiteStore) loadPools(ctx context.Context) ([]proxypool.Pool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, admin_url, COALESCE(token, ''),
		COALESCE(socks_host, ''), COALESCE(mode, 'auto'), COALESCE(enabled, 1)
		FROM gateway_proxy_pools ORDER BY id`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, fmt.Errorf("load proxy pools: %w", err)
	}
	defer rows.Close()
	pools := make([]proxypool.Pool, 0)
	for rows.Next() {
		var pool proxypool.Pool
		var enabled int
		if err := rows.Scan(&pool.ID, &pool.Name, &pool.AdminURL, &pool.Token,
			&pool.SocksHost, &pool.Mode, &enabled); err != nil {
			return nil, fmt.Errorf("scan proxy pool: %w", err)
		}
		pool.Enabled = enabled != 0
		pools = append(pools, pool)
	}
	return pools, rows.Err()
}

// LoadPoolAssignments implements proxypool.Store.
func (s *SQLiteStore) LoadPoolAssignments(ctx context.Context) ([]proxypool.Assignment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pool_id, owner_key, lease_id, ipv6, port,
		site_id, socks_url, updated_at FROM gateway_pool_assignments`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, fmt.Errorf("load pool assignments: %w", err)
	}
	defer rows.Close()
	assignments := make([]proxypool.Assignment, 0)
	for rows.Next() {
		var assignment proxypool.Assignment
		var updatedAt string
		if err := rows.Scan(&assignment.PoolID, &assignment.OwnerKey, &assignment.LeaseID,
			&assignment.IPv6, &assignment.Port, &assignment.SiteID, &assignment.SocksURL,
			&updatedAt); err != nil {
			return nil, fmt.Errorf("scan pool assignment: %w", err)
		}
		if parsed, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
			assignment.UpdatedAt = parsed.UTC()
		}
		assignments = append(assignments, assignment)
	}
	return assignments, rows.Err()
}

// SavePoolAssignments implements proxypool.Store. The manager owns the whole set,
// so the table is replaced with what it holds: an assignment it dropped is one an
// operator released.
func (s *SQLiteStore) SavePoolAssignments(ctx context.Context, assignments []proxypool.Assignment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pool assignment save: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_pool_assignments`); err != nil {
		return fmt.Errorf("clear pool assignments: %w", err)
	}
	for _, assignment := range assignments {
		updatedAt := assignment.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = time.Now()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_pool_assignments
			(pool_id, owner_key, lease_id, ipv6, port, site_id, socks_url, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			assignment.PoolID, assignment.OwnerKey, assignment.LeaseID, assignment.IPv6,
			assignment.Port, assignment.SiteID, assignment.SocksURL,
			updatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("store pool assignment: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pool assignments: %w", err)
	}
	return nil
}

// deletePoolAssignmentsForPool removes every lease assignment of a pool that is
// being deleted: an assignment must never point at a pool that is gone.
func deletePoolAssignmentsForPool(ctx context.Context, tx *sql.Tx, poolID int64) (int64, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM gateway_pool_assignments WHERE pool_id = ?`, poolID)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, fmt.Errorf("delete pool assignments: %w", err)
	}
	return result.RowsAffected()
}
