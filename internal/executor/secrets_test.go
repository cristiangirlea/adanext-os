package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func TestTheAgentsChangesAreScannedForSecretsBeforeReviewWithoutKeepingTheValue(t *testing.T) {
	e, s, _, _, _ := workspaceExecutor(t, "agent_secret")
	e.cfg.CancelGrace = naturalExitGrace
	e.LaunchAttempt("attempt")
	task := waitTaskState(t, s, store.AwaitingReview)
	scan, err := s.SecretScan(context.Background(), "u", "task", "attempt")
	if err != nil || scan == nil {
		t.Fatalf("scan = %+v, %v", scan, err)
	}
	if scan.Commit != task.Attempt.TestedCommit || scan.RulesVersion != source.SecretRulesVersion || scan.Failed || len(scan.NotScanned) != 0 {
		t.Fatalf("scan = %+v (tested %s)", scan, task.Attempt.TestedCommit)
	}
	got := []string{}
	for _, f := range scan.Findings {
		got = append(got, f.Path+":"+f.Rule)
	}
	if strings.Join(got, ",") != ".env:key-file,a.txt:access-key-id" {
		t.Fatalf("findings %v", got)
	}
	ctx := context.Background()
	raw, err := s.Do(ctx, "u", "read-scan", "read", nil, func(tx *store.Tx) ([]byte, error) {
		var data string
		err := tx.QueryRowContext(ctx, "SELECT scan_json FROM secret_scans WHERE attempt='attempt'").Scan(&data)
		return []byte(data), err
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), strings.Repeat("Q", 16)) {
		t.Fatalf("the stored scan holds the value: %s", raw)
	}
}
