package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func recordScan(t *testing.T, s *Store, commit string, scan SecretScan) {
	t.Helper()
	if err := s.write(ctx, "", func(tx *Tx) error { return tx.RecordSecretScan(ctx, "ra", commit, scan) }); err != nil {
		t.Fatal(err)
	}
}

func twoFindings() SecretScan {
	return SecretScan{RulesVersion: "1", Findings: []SecretFinding{
		{ID: "s1", Path: "fixtures/keys.txt", Line: 3, Rule: "access-key-id"},
		{ID: "s2", Path: "config/prod.env", Line: 1, Rule: "env-secret"},
	}}
}

func acceptWith(s *Store, user string, acks []SecretAck) (Review, error) {
	var review Review
	err := s.write(ctx, user, func(tx *Tx) error {
		var err error
		review, err = tx.Accept(ctx, "rt", 4, agentCommit, 3, agentCommit, acks)
		return err
	})
	return review, err
}

func TestAnAcceptWithOpenSecretFindingsIsRefusedUntilEachIsAcknowledged(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	recordScan(t, s, agentCommit, twoFindings())
	_, err := acceptWith(s, "actor", nil)
	var open *SecretsUnacknowledgedError
	if !errors.As(err, &open) || len(open.Findings) != 2 {
		t.Fatalf("accept with no acknowledgement: %v", err)
	}
	_, err = acceptWith(s, "actor", []SecretAck{{Finding: "s1", Kind: "real"}})
	if !errors.As(err, &open) || len(open.Findings) != 1 || open.Findings[0].ID != "s2" {
		t.Fatalf("accept with one of two: %v", err)
	}
	review, err := acceptWith(s, "actor", []SecretAck{{Finding: "s1", Kind: "test", Reason: " a fixture for the parser "}, {Finding: "s2", Kind: "real"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Secrets) != 2 || review.Secrets[0].Path != "fixtures/keys.txt" || review.Secrets[0].Reason != "a fixture for the parser" || review.Secrets[1].Kind != "real" || review.Secrets[1].Rule != "env-secret" {
		t.Fatalf("acknowledgements recorded: %+v", review.Secrets)
	}
	var stored string
	if err := s.db.QueryRowContext(ctx, "SELECT secrets_json FROM reviews WHERE id=?", review.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var again []SecretAck
	if err := json.Unmarshal([]byte(stored), &again); err != nil || len(again) != 2 {
		t.Fatalf("stored with the review: %s %v", stored, err)
	}
	var reason, author string
	if err := s.db.QueryRowContext(ctx, "SELECT reason,author FROM secret_allowlist WHERE project='rp' AND path='fixtures/keys.txt' AND rule='access-key-id'").Scan(&reason, &author); err != nil || reason != "a fixture for the parser" || author != "actor" {
		t.Fatalf("allow-list entry: %q %q %v", reason, author, err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM secret_allowlist WHERE path='config/prod.env'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("a real secret was allow-listed: %d %v", count, err)
	}
	// The test value is not reported again; the real secret still is.
	scan, err := s.SecretScan(ctx, "reviewer", "rt", "ra")
	if err != nil || len(scan.Findings) != 1 || scan.Findings[0].ID != "s2" {
		t.Fatalf("scan after accept = %+v %v", scan, err)
	}
}

func TestATestValueAndARealSecretOfOnePathAndRuleAreRefusedTogether(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	recordScan(t, s, agentCommit, SecretScan{RulesVersion: "1", Findings: []SecretFinding{
		{ID: "s1", Path: "a.go", Line: 3, Rule: "api-key"},
		{ID: "s2", Path: "a.go", Line: 9, Rule: "api-key"},
	}})
	_, err := acceptWith(s, "actor", []SecretAck{{Finding: "s1", Kind: "test", Reason: "fixture"}, {Finding: "s2", Kind: "real"}})
	if !errors.Is(err, ErrInvalidSecretAck) {
		t.Fatalf("mixed answers for one path and rule: %v", err)
	}
	if _, err := acceptWith(s, "actor", []SecretAck{{Finding: "s1", Kind: "test", Reason: "fixture"}, {Finding: "s2", Kind: "test", Reason: "fixture"}}); err != nil {
		t.Fatalf("both test values: %v", err)
	}
}

func TestANonUTF8PathIsAllowListedByItsBytes(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	recordScan(t, s, agentCommit, SecretScan{RulesVersion: "1", Findings: []SecretFinding{
		{ID: "s1", Path: "a\ufffd.txt", PathRaw: "Yf8udHh0", Line: 1, Rule: "jwt"},
		{ID: "s2", Path: "a\ufffd.txt", PathRaw: "Yf4udHh0", Line: 1, Rule: "jwt"},
	}})
	if _, err := acceptWith(s, "actor", []SecretAck{{Finding: "s1", Kind: "test", Reason: "fixture"}, {Finding: "s2", Kind: "real"}}); err != nil {
		t.Fatalf("two paths with the same text but other bytes: %v", err)
	}
	scan, err := s.SecretScan(ctx, "actor", "rt", "ra")
	if err != nil || len(scan.Findings) != 1 || scan.Findings[0].ID != "s2" {
		t.Fatalf("scan after accept = %+v %v", scan, err)
	}
}

func TestATestValueWithoutAReasonAndAnUnknownOrRepeatedAcknowledgementAreRefused(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	recordScan(t, s, agentCommit, twoFindings())
	for name, acks := range map[string][]SecretAck{
		"no reason":    {{Finding: "s1", Kind: "test", Reason: "   "}, {Finding: "s2", Kind: "real"}},
		"unknown id":   {{Finding: "s9", Kind: "real"}, {Finding: "s1", Kind: "real"}, {Finding: "s2", Kind: "real"}},
		"repeated":     {{Finding: "s1", Kind: "real"}, {Finding: "s1", Kind: "real"}, {Finding: "s2", Kind: "real"}},
		"unknown kind": {{Finding: "s1", Kind: "ignore"}, {Finding: "s2", Kind: "real"}},
		"nul reason":   {{Finding: "s1", Kind: "test", Reason: "a\x00b"}, {Finding: "s2", Kind: "real"}},
		"long reason":  {{Finding: "s1", Kind: "test", Reason: strings.Repeat("r", MaxSecretReasonBytes+1)}, {Finding: "s2", Kind: "real"}},
	} {
		if _, err := acceptWith(s, "actor", acks); !errors.Is(err, ErrInvalidSecretAck) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM secret_allowlist").Scan(&count); err != nil || count != 0 {
		t.Fatalf("a refused accept wrote the allow-list: %d %v", count, err)
	}
}

func TestAnAllowListedPathAndRuleIsNotReportedAgain(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	execute(t, s, `INSERT INTO secret_allowlist(project,path,rule,reason,author,created) VALUES ('rp','fixtures/keys.txt','access-key-id','fixture','owner','now')`)
	recordScan(t, s, agentCommit, twoFindings())
	scan, err := s.SecretScan(ctx, "reviewer", "rt", "ra")
	if err != nil || scan == nil || len(scan.Findings) != 1 || scan.Findings[0].ID != "s2" || scan.Commit != agentCommit {
		t.Fatalf("scan read = %+v, %v", scan, err)
	}
	if _, err := acceptWith(s, "actor", []SecretAck{{Finding: "s2", Kind: "real"}}); err != nil {
		t.Fatalf("accept answering only the open finding: %v", err)
	}
	if _, err := s.SecretScan(ctx, "outsider", "rt", "ra"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("a non-member read the scan: %v", err)
	}
}

func TestAnAllowListEntryWithoutAReasonIsRefusedByTheSchema(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO secret_allowlist(project,path,rule,reason,author,created) VALUES ('rp','a','jwt','  ','owner','now')`); err == nil {
		t.Fatal("an entry with a blank reason was stored")
	}
}

func TestAnAttemptWithNoScanNeedsNoAcknowledgementAndRefusesOne(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	if _, err := acceptWith(s, "actor", []SecretAck{{Finding: "s1", Kind: "real"}}); !errors.Is(err, ErrInvalidSecretAck) {
		t.Fatalf("an acknowledgement with no scan: %v", err)
	}
	review, err := acceptWith(s, "actor", nil)
	if err != nil || review.Secrets == nil || len(review.Secrets) != 0 {
		t.Fatalf("accept of an unscanned attempt: %+v %v", review, err)
	}
}

func TestAnAllowListedRawPathDoesNotCoverAUTF8PathSpelledLikeIt(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	recordScan(t, s, agentCommit, SecretScan{RulesVersion: "1", Findings: []SecretFinding{
		{ID: "s1", Path: "a\ufffd.txt", PathRaw: "Yf8udHh0", Line: 1, Rule: "jwt"},
		{ID: "s2", Path: "raw:Yf8udHh0", Line: 1, Rule: "jwt"},
		{ID: "s3", Path: "a\ufffd.txt", Line: 1, Rule: "jwt"},
	}})
	if _, err := acceptWith(s, "actor", []SecretAck{{Finding: "s1", Kind: "test", Reason: "fixture"}, {Finding: "s2", Kind: "real"}, {Finding: "s3", Kind: "real"}}); err != nil {
		t.Fatal(err)
	}
	scan, err := s.SecretScan(ctx, "actor", "rt", "ra")
	if err != nil || len(scan.Findings) != 2 || scan.Findings[0].ID != "s2" || scan.Findings[1].ID != "s3" {
		t.Fatalf("scan after accept = %+v %v", scan, err)
	}
}

func TestAScanOfAnotherCommitRefusesTheAccept(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	recordScan(t, s, otherCommit, twoFindings())
	if _, err := acceptWith(s, "actor", nil); !errors.Is(err, ErrStaleSecretScan) {
		t.Fatalf("accept with a scan of another commit: %v", err)
	}
}

func TestAScanIsReadOnlyThroughItsOwnTask(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	recordScan(t, s, agentCommit, twoFindings())
	execute(t, s, `INSERT INTO tasks(id,project,created_by,objective,acceptance_criteria,state,revision,created,updated) VALUES ('other','rp','creator','o','c','draft',0,'now','now')`)
	scan, err := s.SecretScan(ctx, "reviewer", "other", "ra")
	if err != nil || scan != nil {
		t.Fatalf("another task's attempt read through this one: %+v %v", scan, err)
	}
}

func TestReviewsFromBeforeScansReadAsAnsweringNothing(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, Accepted, "ended_ok")
	path := s.path
	execute(t, s, `INSERT INTO reviews(id,task,attempt,reviewer,decision,reviewed_commit,ownership_generation,checks_json,secrets_json,created) VALUES ('rev_old','rt','ra','actor','accepted',?,3,'[]','[]','now')`, agentCommit)
	for _, q := range []string{"DROP TABLE secret_allowlist", "DROP TABLE secret_scans", "ALTER TABLE reviews DROP COLUMN secrets_json", "DELETE FROM schema_migrations WHERE version>=11", "PRAGMA user_version=10"} {
		execute(t, s, q)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var answered string
	if err := upgraded.db.QueryRowContext(ctx, "SELECT secrets_json FROM reviews WHERE id='rev_old'").Scan(&answered); err != nil || answered != "[]" {
		t.Fatalf("an older review = %q %v", answered, err)
	}
}

func TestAnAcceptOfACommitWhoseScanStoppedRecordingIsRefused(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	scan := twoFindings()
	scan.FindingsCapped = true
	recordScan(t, s, agentCommit, scan)
	if _, err := acceptWith(s, "actor", []SecretAck{{Finding: "s1", Kind: "real"}, {Finding: "s2", Kind: "real"}}); !errors.Is(err, ErrTooManySecretFindings) {
		t.Fatalf("accept of a capped scan: %v", err)
	}
}

func TestASecondScanOfTheAttemptReplacesTheFirst(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	recordScan(t, s, otherCommit, twoFindings())
	recordScan(t, s, agentCommit, SecretScan{RulesVersion: "1"})
	scan, err := s.SecretScan(ctx, "actor", "rt", "ra")
	if err != nil || scan.Commit != agentCommit || len(scan.Findings) != 0 || scan.NotScanned == nil {
		t.Fatalf("scan = %+v %v", scan, err)
	}
}
