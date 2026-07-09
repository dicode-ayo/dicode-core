package db

import (
	"context"
	"strings"
	"testing"
)

// The resume-token lookup and the deadline sweep must be index-backed, not
// full-table scans, since `runs` grows unbounded with history. Assert the
// query planner uses the resume indexes for both hot paths.
func TestResumeIndexes_UsedByPlanner(t *testing.T) {
	d := newTestDB(t).(*SQLiteDB)
	ctx := context.Background()

	cases := []struct {
		name  string
		query string
		index string
	}{
		{
			name:  "resume-token lookup",
			query: `SELECT id FROM runs WHERE resume_token = 'tok'`,
			index: "idx_runs_resume_token",
		},
		{
			name: "deadline sweep",
			query: `SELECT id FROM runs WHERE status = 'suspended' AND resume_deadline IS NOT NULL ` +
				`AND resume_deadline > 0 AND resume_deadline < 123`,
			index: "idx_runs_status_resume_deadline",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := d.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan strings.Builder
			for rows.Next() {
				var a, b, c int
				var detail string
				if err := rows.Scan(&a, &b, &c, &detail); err != nil {
					t.Fatal(err)
				}
				plan.WriteString(detail)
				plan.WriteString("\n")
			}
			got := plan.String()
			if !strings.Contains(got, tc.index) {
				t.Errorf("query plan does not use %s:\n%s", tc.index, got)
			}
			if strings.Contains(got, "SCAN runs") {
				t.Errorf("query plan full-scans runs instead of using %s:\n%s", tc.index, got)
			}
		})
	}
}
