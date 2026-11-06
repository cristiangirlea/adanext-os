package api

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/store"
)

func (f *reviewFixture) recordScan(t *testing.T, commit string, scan store.SecretScan) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.s.Do(ctx, "alice", "scan-"+commit, "scan", nil, func(tx *store.Tx) ([]byte, error) {
		return []byte("ok"), tx.RecordSecretScan(ctx, "ra", commit, scan)
	}); err != nil {
		t.Fatal(err)
	}
}

func scanWithTwoFindings() store.SecretScan {
	return store.SecretScan{RulesVersion: "1", Findings: []store.SecretFinding{
		{ID: "s1", Path: "fixtures/keys.txt", Line: 3, Rule: "access-key-id"},
		{ID: "s2", Path: "deploy/.env", Line: 0, Rule: "key-file"},
	}, NotScanned: []store.SecretNotScanned{{Path: "logo.png", Reason: "binary"}}}
}

func TestChangesShowTheSecretScanOfTheReviewedCommit(t *testing.T) {
	f := newReviewFixture(t, store.AwaitingReview, "ended_ok", reviewedCommit)
	status, data := f.do(t, "dave", "GET", "/v1/tasks/rt/changes", "", "", nil)
	if status != 200 || !bytes.Contains(data, []byte(`"secrets":null`)) {
		t.Fatalf("an unscanned attempt = %d %s", status, data)
	}
	f.recordScan(t, reviewedCommit, scanWithTwoFindings())
	status, data = f.do(t, "dave", "GET", "/v1/tasks/rt/changes", "", "", nil)
	got := decodeJSON[changesJSON](t, data)
	if status != 200 || got.Secrets == nil || len(got.Secrets.Findings) != 2 || got.Secrets.Findings[0].Rule != "access-key-id" || got.Secrets.Findings[1].Line != 0 ||
		len(got.Secrets.NotScanned) != 1 || got.Secrets.NotScanned[0].Reason != "binary" || got.Secrets.Commit != reviewedCommit {
		t.Fatalf("changes = %d %s", status, data)
	}
}

func TestAnAcceptAnswersEachSecretFindingOverHTTP(t *testing.T) {
	f := newReviewFixture(t, store.AwaitingReview, "ended_ok", reviewedCommit)
	f.setTip(reviewedCommit)
	f.recordScan(t, reviewedCommit, scanWithTwoFindings())
	body := map[string]any{"commit": reviewedCommit, "generation": 0}
	status, data := f.do(t, "erin", "POST", "/v1/tasks/rt/accept", "none", "4", body)
	if status != 409 || !bytes.Contains(data, []byte(`"secrets_unacknowledged"`)) || !bytes.Contains(data, []byte(`"id":"s1"`)) || !bytes.Contains(data, []byte(`"id":"s2"`)) {
		t.Fatalf("accept with open findings = %d %s", status, data)
	}
	body["secrets"] = []map[string]any{{"finding": "s1", "kind": "test"}, {"finding": "s2", "kind": "real"}}
	if status, data := f.do(t, "erin", "POST", "/v1/tasks/rt/accept", "no-reason", "4", body); status != 400 || !bytes.Contains(data, []byte(`"bad_request"`)) {
		t.Fatalf("a test value with no reason = %d %s", status, data)
	}
	body["secrets"] = []map[string]any{{"finding": "s1", "kind": "test", "reason": "parser fixture"}, {"finding": "s2", "kind": "real"}}
	status, data = f.do(t, "erin", "POST", "/v1/tasks/rt/accept", "answered", "4", body)
	if status != 200 || !bytes.Contains(data, []byte(`"state":"accepted"`)) || !bytes.Contains(data, []byte(`"kind":"real"`)) || !bytes.Contains(data, []byte(`"reason":"parser fixture"`)) {
		t.Fatalf("accept with each finding answered = %d %s", status, data)
	}
}

func TestAnAnsweredAcceptReplaysByItsKeyAfterItWroteTheAllowList(t *testing.T) {
	f := newReviewFixture(t, store.AwaitingReview, "ended_ok", reviewedCommit)
	f.setTip(reviewedCommit)
	f.recordScan(t, reviewedCommit, scanWithTwoFindings())
	body := map[string]any{"commit": reviewedCommit, "generation": 0, "secrets": []map[string]any{{"finding": "s1", "kind": "test", "reason": "fixture"}, {"finding": "s2", "kind": "real"}}}
	status, first := f.do(t, "erin", "POST", "/v1/tasks/rt/accept", "once", "4", body)
	if status != 200 {
		t.Fatalf("accept = %d %s", status, first)
	}
	if status, again := f.do(t, "erin", "POST", "/v1/tasks/rt/accept", "once", "4", body); status != 200 || !bytes.Equal(again, first) {
		t.Fatalf("replay = %d %s", status, again)
	}
}

func TestChangesMarkAScanOfAnotherCommitAsCoveringNothingAndAcceptRefusesIt(t *testing.T) {
	f := newReviewFixture(t, store.AwaitingReview, "ended_ok", reviewedCommit)
	f.setTip(reviewedCommit)
	f.recordScan(t, strings.Repeat("e", 40), scanWithTwoFindings())
	status, data := f.do(t, "dave", "GET", "/v1/tasks/rt/changes", "", "", nil)
	got := decodeJSON[changesJSON](t, data)
	if status != 200 || got.Secrets == nil || !got.Secrets.Failed || got.Secrets.Commit != reviewedCommit || len(got.Secrets.Findings) != 0 {
		t.Fatalf("changes = %d %s", status, data)
	}
	if status, data := f.do(t, "erin", "POST", "/v1/tasks/rt/accept", "stale-scan", "4", map[string]any{"commit": reviewedCommit, "generation": 0}); status != 409 || !bytes.Contains(data, []byte(`"secret_scan_stale"`)) {
		t.Fatalf("accept with a stale scan = %d %s", status, data)
	}
}

func TestAnAcceptOfAScanThatStoppedRecordingIsRefusedOverHTTP(t *testing.T) {
	f := newReviewFixture(t, store.AwaitingReview, "ended_ok", reviewedCommit)
	f.setTip(reviewedCommit)
	scan := scanWithTwoFindings()
	scan.FindingsCapped = true
	f.recordScan(t, reviewedCommit, scan)
	body := map[string]any{"commit": reviewedCommit, "generation": 0, "secrets": []map[string]any{{"finding": "s1", "kind": "real"}, {"finding": "s2", "kind": "real"}}}
	if status, data := f.do(t, "erin", "POST", "/v1/tasks/rt/accept", "capped", "4", body); status != 409 || !bytes.Contains(data, []byte(`"too_many_secret_findings"`)) {
		t.Fatalf("accept of a capped scan = %d %s", status, data)
	}
}
