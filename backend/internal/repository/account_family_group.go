package repository

import (
	"context"

	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

var _ service.AccountFamilyGroupReader = (*accountRepository)(nil)

func filterAccountFamilyGroup(ctx context.Context, query *dbent.AccountQuery) *dbent.AccountQuery {
	name := service.AccountFamilyGroupFilter(ctx)
	if name == "" {
		return query
	}
	return query.Where(func(s *entsql.Selector) {
		path := sqljson.Path(service.AccountFamilyGroupKey)
		if name == service.AccountFamilyGroupUnset {
			s.Where(entsql.Or(entsql.Not(sqljson.HasKey(dbaccount.FieldExtra, path)), sqljson.ValueIsNull(dbaccount.FieldExtra, path), sqljson.ValueEQ(dbaccount.FieldExtra, "", path)))
		} else {
			s.Where(sqljson.ValueEQ(dbaccount.FieldExtra, name, path))
		}
	})
}

// Aggregate only names and counts; never load credentials to populate selectors.
func (r *accountRepository) ListAccountFamilyGroups(ctx context.Context) ([]service.AccountFamilyGroup, error) {
	query := `SELECT extra->>'family_group' AS name, COUNT(*)
 FROM accounts a WHERE a.deleted_at IS NULL
 AND jsonb_typeof(a.extra->'family_group') = 'string'
 AND a.extra->>'family_group' <> ''`
	args := []any{}
	if ids, scoped := service.ObserverGroupIDs(ctx); scoped {
		query += ` AND EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id = a.id AND ag.group_id = ANY($1))`
		args = append(args, pq.Array(ids))
	}
	query += ` GROUP BY extra->>'family_group' ORDER BY name`
	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]service.AccountFamilyGroup, 0)
	for rows.Next() {
		var group service.AccountFamilyGroup
		if err := rows.Scan(&group.Name, &group.AccountCount); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}
