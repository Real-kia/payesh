package monitoring

import (
	"strings"
	"testing"
	"time"
)

// Fleet reads must search recent timestamps rather than scanning every stored
// sample in server order. This regresses as the database accumulates history.
func TestOverviewReadsOnlyIndexedTimeRange(t *testing.T) {
	s := openDiskStore(t)
	for _, ids := range [][]string{nil, {"server-local-0001"}} {
		query, args := overviewQuery(time.Now(), ids)
		rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var searched bool
		for rows.Next() {
			var a, b, c int
			var detail string
			if err := rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(detail, "SCAN metric_samples") || strings.Contains(detail, "USE TEMP B-TREE") {
				t.Errorf("unbounded overview query: %s", detail)
			}
			if strings.Contains(detail, "SEARCH metric_samples") && strings.Contains(detail, "observed_at>?") {
				searched = true
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !searched {
			t.Fatal("overview did not search the indexed time range")
		}
	}
}
