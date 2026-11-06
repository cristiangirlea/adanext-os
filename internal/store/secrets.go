package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrStaleSecretScan is an accept of a commit the attempt's recorded scan does not cover.
	ErrStaleSecretScan = errors.New("the secret scan is of another commit than the one reviewed")
	// ErrInvalidSecretAck is an acknowledgement of no open finding, a second one for the same
	// finding, an unknown kind, or a test value without a reason.
	ErrInvalidSecretAck = errors.New("invalid acknowledgement of a secret finding")
	// ErrTooManySecretFindings is an accept of a commit whose scan stopped recording findings.
	ErrTooManySecretFindings = errors.New("the scan found more possible secrets than it records; refine to remove them")
)

// MaxSecretReasonBytes bounds the reason given with an acknowledgement.
const MaxSecretReasonBytes = 1 << 10

// SecretFinding is a line that looks like a credential: where and which rule, never the value.
// PathRaw holds a path's bytes in base64 when they are not UTF-8.
type SecretFinding struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	PathRaw string `json:"path_raw,omitempty"`
	Line    int    `json:"line"`
	Rule    string `json:"rule"`
}

// allowKey is what an allow-list entry matches: the path, its raw bytes when they are not UTF-8,
// and the rule.
func (f SecretFinding) allowKey() [3]string { return [3]string{f.Path, f.PathRaw, f.Rule} }

// SecretNotScanned is a file a scan could not read in full.
type SecretNotScanned struct {
	Path    string `json:"path"`
	PathRaw string `json:"path_raw,omitempty"`
	Reason  string `json:"reason"`
}

// SecretScan is the scan of an attempt's changes at Commit. Findings are those still open: an
// allow-listed path and rule is left out when read through SecretScan or Accept.
type SecretScan struct {
	Commit         string             `json:"commit"`
	RulesVersion   string             `json:"rules_version"`
	Findings       []SecretFinding    `json:"findings"`
	FindingsCapped bool               `json:"findings_capped"`
	NotScanned     []SecretNotScanned `json:"not_scanned"`
	Unlisted       int                `json:"unlisted"`
	Capped         bool               `json:"capped"`
	Failed         bool               `json:"failed"`
}

// SecretAck answers one open finding at accept: kind test (allow-listed with Reason) or real
// (the user rotates it; export refuses the branch while it holds the secret).
type SecretAck struct {
	Finding string `json:"finding"`
	Kind    string `json:"kind"`
	Reason  string `json:"reason,omitempty"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Rule    string `json:"rule"`
}

// SecretsUnacknowledgedError is an accept that left open findings unanswered.
type SecretsUnacknowledgedError struct{ Findings []SecretFinding }

func (e *SecretsUnacknowledgedError) Error() string {
	return "the changes hold possible secrets; acknowledge each to accept"
}

// RecordSecretScan records the scan of an attempt's changes at commit, replacing an earlier one.
func (tx *Tx) RecordSecretScan(ctx context.Context, attemptID, commit string, scan SecretScan) error {
	if scan.Findings == nil {
		scan.Findings = []SecretFinding{}
	}
	if scan.NotScanned == nil {
		scan.NotScanned = []SecretNotScanned{}
	}
	scan.Commit = commit
	data, err := json.Marshal(scan)
	if err != nil {
		return err
	}
	_, err = tx.tx.ExecContext(ctx, `INSERT INTO secret_scans(attempt,scanned_commit,rules_version,scan_json,created) VALUES(?,?,?,?,?)
		ON CONFLICT(attempt) DO UPDATE SET scanned_commit=excluded.scanned_commit,rules_version=excluded.rules_version,scan_json=excluded.scan_json,created=excluded.created`,
		attemptID, commit, scan.RulesVersion, string(data), now())
	return err
}

// SecretScan reads the scan of an attempt of the task for a current member, its allow-listed
// findings left out; nil when that attempt has none or is not the task's.
func (s *Store) SecretScan(ctx context.Context, userID, taskID, attemptID string) (*SecretScan, error) {
	r, err := s.ReviewState(ctx, userID, taskID)
	if err != nil {
		return nil, err
	}
	return openSecretScan(ctx, s.db, r.Project, taskID, attemptID)
}

func openSecretScan(ctx context.Context, q querier, project, taskID, attemptID string) (*SecretScan, error) {
	var data string
	err := q.QueryRowContext(ctx, "SELECT s.scan_json FROM secret_scans s JOIN attempts a ON a.id=s.attempt WHERE s.attempt=? AND a.task=?", attemptID, taskID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var scan SecretScan
	if err := json.Unmarshal([]byte(data), &scan); err != nil {
		return nil, err
	}
	allowed := map[[3]string]bool{}
	rows, err := q.QueryContext(ctx, "SELECT path,path_raw,rule FROM secret_allowlist WHERE project=?", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p, raw, rule string
		if err := rows.Scan(&p, &raw, &rule); err != nil {
			return nil, err
		}
		allowed[[3]string{p, raw, rule}] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	open := []SecretFinding{}
	for _, f := range scan.Findings {
		if !allowed[f.allowKey()] {
			open = append(open, f)
		}
	}
	scan.Findings = open
	if scan.NotScanned == nil {
		scan.NotScanned = []SecretNotScanned{}
	}
	return &scan, nil
}

// answerSecrets checks that acks answer each open finding of the scan of commit exactly once,
// writes the allow-list entries of the test values, and returns the acknowledgements to record.
func (tx *Tx) answerSecrets(ctx context.Context, project, taskID, attemptID, commit string, acks []SecretAck) ([]SecretAck, error) {
	scan, err := openSecretScan(ctx, tx.tx, project, taskID, attemptID)
	if err != nil {
		return nil, err
	}
	if scan == nil {
		// An attempt from before scans existed: the review says it was not scanned.
		if len(acks) > 0 {
			return nil, ErrInvalidSecretAck
		}
		return []SecretAck{}, nil
	}
	if scan.Commit != commit {
		return nil, ErrStaleSecretScan
	}
	if scan.FindingsCapped {
		return nil, ErrTooManySecretFindings
	}
	open := map[string]SecretFinding{}
	for _, f := range scan.Findings {
		open[f.ID] = f
	}
	answered := map[string]bool{}
	kinds := map[[3]string]string{}
	raws := map[string]string{}
	out := []SecretAck{}
	for _, a := range acks {
		f, ok := open[a.Finding]
		if !ok || answered[a.Finding] || len(a.Reason) > MaxSecretReasonBytes || strings.ContainsRune(a.Reason, 0) {
			return nil, ErrInvalidSecretAck
		}
		reason := strings.TrimSpace(a.Reason)
		switch a.Kind {
		case "test":
			if reason == "" {
				return nil, fmt.Errorf("%w: a test value needs a reason", ErrInvalidSecretAck)
			}
		case "real":
		default:
			return nil, ErrInvalidSecretAck
		}
		// One allow-list entry covers every finding of its path and rule, so they share a kind.
		if k, ok := kinds[f.allowKey()]; ok && k != a.Kind {
			return nil, fmt.Errorf("%w: a test value and a real secret share %s and rule %s", ErrInvalidSecretAck, f.Path, f.Rule)
		}
		kinds[f.allowKey()] = a.Kind
		raws[f.ID] = f.PathRaw
		answered[a.Finding] = true
		out = append(out, SecretAck{Finding: f.ID, Kind: a.Kind, Reason: reason, Path: f.Path, Line: f.Line, Rule: f.Rule})
	}
	if len(answered) != len(open) {
		left := []SecretFinding{}
		for _, f := range scan.Findings {
			if !answered[f.ID] {
				left = append(left, f)
			}
		}
		return nil, &SecretsUnacknowledgedError{Findings: left}
	}
	for _, a := range out {
		if a.Kind != "test" {
			continue
		}
		if _, err := tx.tx.ExecContext(ctx, "INSERT INTO secret_allowlist(project,path,path_raw,rule,reason,author,created) VALUES(?,?,?,?,?,?,?) ON CONFLICT(project,path,path_raw,rule) DO NOTHING",
			project, a.Path, raws[a.Finding], a.Rule, a.Reason, tx.userID, now()); err != nil {
			return nil, err
		}
	}
	return out, nil
}
