package source

import (
	"bytes"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// SecretRulesVersion names the rule set below; every scan records it. Change it with the rules.
const SecretRulesVersion = "1"

// MaxSecretFindings bounds the findings one scan records; past it the scan says so.
const MaxSecretFindings = 1000

// SecretScanLimits are the caps a scan reads the change set with: past them a file is reported
// as not scanned.
var SecretScanLimits = ChangeLimits{Files: 5000, FileBytes: 1 << 20, TotalBytes: 32 << 20}

// SecretFinding is a line added in a change set that looks like a credential. It holds where and
// which rule, never the matched value. Line 0 is the whole file.
type SecretFinding struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Rule string `json:"rule"`
}

// NotScanned is a file a scan could not read in full: binary, or past SecretScanLimits.
type NotScanned struct {
	Path   string `json:"path"`
	Reason string `json:"reason"` // binary or too_large
}

// SecretScan is what a scan of a change set found. Unlisted counts files past the listing cap;
// Capped means even that count stopped short; Failed means the scan did not run.
type SecretScan struct {
	RulesVersion   string          `json:"rules_version"`
	Findings       []SecretFinding `json:"findings"`
	FindingsCapped bool            `json:"findings_capped"` // more findings than MaxSecretFindings
	NotScanned     []NotScanned    `json:"not_scanned"`
	Unlisted       int             `json:"unlisted"`
	Capped         bool            `json:"capped"`
	Failed         bool            `json:"failed"`
}

// Complete reports a scan that read every added line and recorded every finding.
func (s SecretScan) Complete() bool {
	return !s.Failed && !s.Capped && !s.FindingsCapped && s.Unlisted == 0 && len(s.NotScanned) == 0
}

type secretRule struct {
	name string
	re   *regexp.Regexp
	// value is the submatch a placeholder check reads; 0 means the rule has no free-form value.
	value int
}

// Formats with a fixed shape, checked first.
var exactRules = []secretRule{
	{name: "access-key-id", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "git-host-token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255})\b`)},
	{name: "private-key", re: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`)},
	{name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{name: "chat-token", re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{name: "api-key", re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)},
	{name: "browser-api-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{name: "live-payment-key", re: regexp.MustCompile(`\b(?:sk|rk)_live_[0-9A-Za-z]{24,}`)},
}

// A secret-looking name given a literal; reported only when no exact rule matched the line.
var assignmentRules = []secretRule{
	{name: "secret-assignment", value: 1, re: regexp.MustCompile(`(?i)(?:secret|token|passw(?:or)?d|pwd|api_?key|access_?key|private_?key)[A-Za-z0-9_]*["']?\s*(?::=|=|:)\s*["']([^"'\s]{8,})["']`)},
	{name: "env-secret", value: 1, re: regexp.MustCompile(`^\s*(?:export\s+)?[A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_?KEY|ACCESS_?KEY|PRIVATE_?KEY)[A-Z0-9_]*\s*=\s*([^\s"'#]{8,})\s*$`)},
}

// templateForm is one whole template reference, with no second one inside it.
var templateForm = regexp.MustCompile(`^(?:\$\{[^${}]*\}|\{\{[^{}]*\}\}|\$\([^$()]*\)|%\([^%()]*\)s|<[^<>]*>)$`)

// placeholder reports a value that as a whole stands for a secret rather than being one: a
// single template reference, a filler of x, * or dots, or one character repeated.
func placeholder(v string) bool {
	return templateForm.MatchString(v) || strings.Trim(v, "xX*.") == "" || strings.Count(v, v[:1]) == len(v)
}

// keyFile reports a file name that holds credentials by its nature.
func keyFile(p string) bool {
	base := path.Base(p)
	switch base {
	case ".env", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519":
		return true
	}
	if rest, ok := strings.CutPrefix(base, ".env."); ok {
		switch rest {
		case "example", "sample", "template", "dist":
			return false
		}
		return true
	}
	switch strings.ToLower(path.Ext(base)) {
	case ".pem", ".key", ".pfx", ".p12", ".jks", ".keystore":
		return true
	}
	return false
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ScanSecrets reads the lines a change set adds, file by file, and reports what looks like a
// credential, files that hold credentials by name, and what it could not read.
func ScanSecrets(set ChangeSet) SecretScan {
	scan := SecretScan{RulesVersion: SecretRulesVersion, Findings: []SecretFinding{}, NotScanned: []NotScanned{}}
	add := func(p string, line int, rule string) {
		if len(scan.Findings) == MaxSecretFindings {
			scan.FindingsCapped = true
			return
		}
		scan.Findings = append(scan.Findings, SecretFinding{ID: "s" + strconv.Itoa(len(scan.Findings)+1), Path: p, Line: line, Rule: rule})
	}
	for _, f := range set.Files {
		if f.Status == "D" {
			continue
		}
		if (f.Status == "A" || f.Status == "R" || f.Status == "C") && keyFile(f.Path) {
			add(f.Path, 0, "key-file")
		}
		if f.Binary {
			scan.NotScanned = append(scan.NotScanned, NotScanned{Path: f.Path, Reason: "binary"})
			continue
		}
		if f.PatchTruncated {
			scan.NotScanned = append(scan.NotScanned, NotScanned{Path: f.Path, Reason: "too_large"})
		}
		scanPatch(f.Path, f.Patch, add)
	}
	if set.FilesTruncated {
		scan.Unlisted = max(0, set.Total-len(set.Files))
	}
	scan.Capped = set.TotalCapped
	return scan
}

// scanPatch walks a file's unified diff by its hunk counts, so header lines of a second patch
// for the same file are never read as added lines.
func scanPatch(p string, patch []byte, add func(string, int, string)) {
	oldLeft, newLeft, line := 0, 0, 0
	for raw := range bytes.SplitSeq(patch, []byte("\n")) {
		text := strings.TrimSuffix(string(raw), "\r")
		if oldLeft <= 0 && newLeft <= 0 {
			m := hunkHeader.FindStringSubmatch(text)
			if m == nil {
				continue
			}
			oldLeft, newLeft = hunkCount(m[1]), hunkCount(m[3])
			line, _ = strconv.Atoi(m[2])
			continue
		}
		if text == "" {
			continue
		}
		switch text[0] {
		case '+':
			newLeft--
			for _, rule := range lineRules(text[1:]) {
				add(p, line, rule)
			}
			line++
		case '-':
			oldLeft--
		case ' ':
			oldLeft--
			newLeft--
			line++
		}
	}
}

func hunkCount(s string) int {
	if s == "" {
		return 1
	}
	n, _ := strconv.Atoi(s)
	return n
}

// lineRules names each match on one added line: every exact match, else the first assignment.
func lineRules(text string) []string {
	var out []string
	for _, r := range exactRules {
		for range r.re.FindAllStringIndex(text, -1) {
			out = append(out, r.name)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, r := range assignmentRules {
		for _, m := range r.re.FindAllStringSubmatch(text, -1) {
			if !placeholder(m[r.value]) {
				return []string{r.name}
			}
		}
	}
	return nil
}
