package availability

import (
	"strings"
	"testing"
)

// Invalidate and the console evidence lookup must find releases by id and by
// checked_id through indexes, not by scanning availability_releases.
func TestReleaseLookupsByCheckedIDUseIndexes(t *testing.T) {
	x := setup(t)
	for name, q := range map[string]string{
		"invalidate-id":      `UPDATE availability_releases SET revision=revision+1 WHERE id=?`,
		"invalidate-checked": `UPDATE availability_releases SET revision=revision+1 WHERE checked_id=?`,
		"console-evidence":   `SELECT id FROM availability_releases WHERE id=? UNION SELECT id FROM availability_releases WHERE checked_id=?`,
	} {
		args := []any{"a"}
		if strings.Contains(q, "UNION") {
			args = append(args, "a")
		}
		plan := queryPlanText(t, x, q, args...)
		for _, line := range strings.Split(plan, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "SCAN availability_releases") {
				t.Errorf("%s scans availability_releases:\n%s", name, plan)
			}
		}
	}
}
