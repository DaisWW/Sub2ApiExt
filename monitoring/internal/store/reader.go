package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/access"
	"github.com/lib/pq"
)

// Reader is the Web service's read-only, request-scoped data boundary. The Worker
// retains Store; no grant is stored on the shared Store or database connection.
type Reader struct {
	store *Store
	scope access.Scope
}

func (s *Store) Read(scope access.Scope) *Reader {
	return &Reader{store: s, scope: scope}
}

// These non-recursive CTEs intentionally shadow the base relations for this one
// SELECT. All existing aggregations, limits and denominators therefore run on
// authorized rows, without rewriting SQL or applying a filter after aggregation.
// PostgreSQL resolves each CTE's own FROM to the underlying relation.
// Reader queries must use these unqualified relation names; schema-qualified
// names bypass CTE visibility. Keep the CTE names reserved when adding queries.
const readScopeQuery = `
WITH monitor_allowed_groups AS NOT MATERIALIZED (
    SELECT g.id FROM groups g
    WHERE g.deleted_at IS NULL AND LOWER(TRIM(g.status)) = 'active'
      AND ((%s::boolean AND COALESCE(g.is_exclusive, FALSE) = FALSE
            AND COALESCE(g.subscription_type, 'standard') <> 'subscription')
           OR g.id = ANY(%s::bigint[]))
), monitor_allowed_accounts AS NOT MATERIALIZED (
    SELECT a.id FROM accounts a
    WHERE EXISTS (SELECT 1 FROM account_groups ag
                  JOIN monitor_allowed_groups g ON g.id = ag.group_id
                  WHERE ag.account_id = a.id)
      AND NOT EXISTS (SELECT 1 FROM account_groups ag
                      WHERE ag.account_id = a.id
                        AND NOT EXISTS (SELECT 1 FROM monitor_allowed_groups g WHERE g.id = ag.group_id))
), usage_logs AS NOT MATERIALIZED (
    SELECT ul.* FROM usage_logs ul
    WHERE ul.group_id IN (SELECT id FROM monitor_allowed_groups)
), ops_error_logs AS NOT MATERIALIZED (
    SELECT oe.* FROM ops_error_logs oe
    WHERE oe.group_id IN (SELECT id FROM monitor_allowed_groups)
), monitoring_targets AS NOT MATERIALIZED (
    SELECT t.target_key, t.kind, t.entity_id, t.name, t.platform, t.source_status,
           t.probe_enabled, t.active, t.source_updated_at,
           CASE WHEN t.kind = 'account' THEN latest.created_at ELSE t.last_activity_at END AS last_activity_at,
           NULL::timestamptz AS last_channel_error_at,
           NULL::text AS last_channel_error_class,
           NULL::integer AS last_channel_error_status_code,
           NULL::timestamptz AS last_channel_error_resolved_at
    FROM monitoring_targets t
    LEFT JOIN LATERAL (
        SELECT MAX(ul.created_at) AS created_at FROM usage_logs ul
        WHERE t.kind = 'account' AND ul.account_id = t.entity_id AND ul.actual_cost > 0
    ) latest ON t.kind = 'account'
    WHERE (t.kind = 'group' AND t.entity_id IN (SELECT id FROM monitor_allowed_groups))
       OR (t.kind = 'account' AND t.entity_id IN (SELECT id FROM monitor_allowed_accounts))
), monitoring_checks AS NOT MATERIALIZED (
    SELECT mc.* FROM monitoring_checks mc
    WHERE mc.target_key IN (SELECT target_key FROM monitoring_targets)
      AND (mc.group_id IS NULL OR mc.group_id IN (SELECT id FROM monitor_allowed_groups))
), monitoring_alerts AS NOT MATERIALIZED (
    SELECT alerts.* FROM monitoring_alerts alerts
    WHERE alerts.target_key IN (SELECT target_key FROM monitoring_targets)
)
`

func (r *Reader) scopedQuery(query string, args []any) (string, []any) {
	if r.scope.IsAdmin() {
		return query, args
	}
	prefix := fmt.Sprintf(readScopeQuery, fmt.Sprintf("$%d", len(args)+1), fmt.Sprintf("$%d", len(args)+2))
	query = strings.TrimSpace(query)
	if strings.HasPrefix(query, "WITH ") {
		query = ", " + strings.TrimPrefix(query, "WITH ")
	}
	return prefix + query, append(args, r.scope.IsPublic(), pq.Array(r.scope.GroupIDs()))
}

func (r *Reader) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	query, args = r.scopedQuery(query, args)
	return r.store.db.QueryContext(ctx, query, args...)
}

func (r *Reader) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	query, args = r.scopedQuery(query, args)
	return r.store.db.QueryRowContext(ctx, query, args...)
}
