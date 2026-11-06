package source

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Sample values are built from pieces so this file holds no string a scanner would report.
var (
	sampleAccessKey = "AKIA" + strings.Repeat("Q", 16)
	sampleGitToken  = "ghp" + "_" + strings.Repeat("a1", 18)
	samplePrivate   = "-----BEGIN " + "RSA PRIVATE KEY-----"
	sampleJWT       = "eyJ" + "hbGciOiJIUzI1NiJ9" + ".eyJ" + "zdWIiOiIxMjM0In0" + ".c2lnbmF0dXJlX3ZhbHVl"
	sampleChat      = "xox" + "b-" + strings.Repeat("1", 12) + "-abcdefgh"
	sampleAPIKey    = "sk" + "-" + strings.Repeat("Z9", 12)
	sampleBrowser   = "AIza" + strings.Repeat("b", 35)
	samplePayment   = "sk" + "_live_" + strings.Repeat("c", 24)
	sampleQuoted    = "s3cr3t-" + "value-42"
	sampleEnv       = "plain" + "envvalue99"
)

func patchOf(lines ...string) []byte {
	return []byte("diff --git a/f b/f\nindex 1..2 100644\n--- a/f\n+++ b/f\n" + strings.Join(lines, "\n") + "\n")
}

func TestScanSecretsFindsEachRuleOnItsAddedLine(t *testing.T) {
	cases := []struct{ line, rule string }{
		{"key := \"" + sampleAccessKey + "\"", "access-key-id"},
		{"token: " + sampleGitToken, "git-host-token"},
		{samplePrivate, "private-key"},
		{"auth = " + sampleJWT, "jwt"},
		{"hook " + sampleChat, "chat-token"},
		{"k = '" + sampleAPIKey + "'", "api-key"},
		{"browser " + sampleBrowser, "browser-api-key"},
		{"pay " + samplePayment, "live-payment-key"},
		{"db_password = \"" + sampleQuoted + "\"", "secret-assignment"},
		{"API_TOKEN=" + sampleEnv, "env-secret"},
	}
	for _, c := range cases {
		set := ChangeSet{Files: []ChangedFile{{Path: "src/a.txt", Status: "M", Patch: patchOf("@@ -10,2 +10,3 @@", " kept", "+"+c.line, " tail")}}}
		scan := ScanSecrets(set)
		if len(scan.Findings) != 1 {
			t.Fatalf("%s: findings %+v", c.rule, scan.Findings)
		}
		f := scan.Findings[0]
		if f.Rule != c.rule || f.Path != "src/a.txt" || f.Line != 11 {
			t.Fatalf("%s: got %+v", c.rule, f)
		}
		if scan.RulesVersion != SecretRulesVersion || f.ID == "" {
			t.Fatalf("version or id missing: %+v", scan)
		}
	}
}

func TestScanSecretsKeepsNoPartOfTheMatchedValue(t *testing.T) {
	lines := []string{"@@ -0,0 +1,3 @@", "+" + sampleAccessKey, "+db_password = \"" + sampleQuoted + "\"", "+API_TOKEN=" + sampleEnv}
	scan := ScanSecrets(ChangeSet{Files: []ChangedFile{{Path: "x.go", Status: "A", Patch: patchOf(lines...)}}})
	if len(scan.Findings) != 3 {
		t.Fatalf("findings %+v", scan.Findings)
	}
	data, err := json.Marshal(scan)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{sampleAccessKey, sampleQuoted, sampleEnv, sampleAccessKey[4:12], sampleQuoted[:6], sampleEnv[:6]} {
		if strings.Contains(string(data), v) {
			t.Fatalf("the scan holds %q: %s", v, data)
		}
	}
}

func TestScanSecretsIgnoresRemovedAndContextLines(t *testing.T) {
	patch := patchOf("@@ -1,2 +1,2 @@", "-"+sampleAccessKey, " "+sampleGitToken, "+clean line")
	scan := ScanSecrets(ChangeSet{Files: []ChangedFile{{Path: "a", Status: "M", Patch: patch}}})
	if len(scan.Findings) != 0 {
		t.Fatalf("findings %+v", scan.Findings)
	}
}

func TestScanSecretsCountsLinesAcrossHunks(t *testing.T) {
	patch := patchOf("@@ -1,1 +1,2 @@", " one", "+two", "@@ -40,2 +41,3 @@", " a", "-b", "+"+sampleAccessKey, "+c")
	scan := ScanSecrets(ChangeSet{Files: []ChangedFile{{Path: "a", Status: "M", Patch: patch}}})
	if len(scan.Findings) != 1 || scan.Findings[0].Line != 42 {
		t.Fatalf("findings %+v", scan.Findings)
	}
}

func TestScanSecretsSkipsPlaceholdersAndCodeReads(t *testing.T) {
	for _, line := range []string{
		`password = "${DB_PASSWORD}"`,
		`token := r.Header.Get("Authorization")`,
		`API_KEY=<your key here>`,
		`secret: "xxxxxxxxxxxx"`,
		`SECRET_TOKEN=${SECRET_TOKEN}`,
		`password = "short"`,
		`API_KEY=<your-key-here>`,
		`db_password = "%(password)s"`,
		`token = "********"`,
	} {
		scan := ScanSecrets(ChangeSet{Files: []ChangedFile{{Path: "a", Status: "M", Patch: patchOf("@@ -1 +1 @@", "+"+line)}}})
		if len(scan.Findings) != 0 {
			t.Fatalf("%q: findings %+v", line, scan.Findings)
		}
	}
}

func TestScanSecretsReportsALiteralThatOnlyContainsAPlaceholderMarker(t *testing.T) {
	for _, line := range []string{
		`password = "correct<horse123"`,
		`secret = "abcxxxxdef99"`,
		`API_TOKEN=prefix${x}suffix99`,
		`password = "<dummy>correcthorse123<dummy>"`,
		`secret = "${A}realvalue99${B}"`,
	} {
		scan := ScanSecrets(ChangeSet{Files: []ChangedFile{{Path: "a", Status: "M", Patch: patchOf("@@ -1 +1 @@", "+"+line)}}})
		if len(scan.Findings) != 1 {
			t.Fatalf("%q: findings %+v", line, scan.Findings)
		}
	}
}

func TestScanSecretsReadsATypeChangesSecondHeaderAsAHeader(t *testing.T) {
	// git writes a type change as a deletion and an addition: two patches for one file.
	patch := []byte("diff --git a/f b/f\ndeleted file mode 120000\nindex 1..0\n--- a/f\n+++ /dev/null\n@@ -1 +0,0 @@\n-target\n\\ No newline at end of file\n" +
		"diff --git a/f b/f\nnew file mode 100644\nindex 0..2\n--- /dev/null\n+++ b/f\n@@ -0,0 +1,2 @@\n+plain\n+" + sampleAccessKey + "\n")
	scan := ScanSecrets(ChangeSet{Files: []ChangedFile{{Path: "f", Status: "T", Patch: patch}}})
	if len(scan.Findings) != 1 || scan.Findings[0].Line != 2 || scan.Findings[0].Rule != "access-key-id" {
		t.Fatalf("findings %+v", scan.Findings)
	}
}

func TestScanSecretsReportsKeyFilesThatAreAdded(t *testing.T) {
	files := []ChangedFile{
		{Path: ".env", Status: "A", Patch: patchOf("@@ -0,0 +1 @@", "+DEBUG=1")},
		{Path: "deploy/id_ed25519", Status: "A", Patch: patchOf("@@ -0,0 +1 @@", "+x")},
		{Path: "certs/server.pem", Status: "R", OldPath: "server.pem.txt", Patch: patchOf()},
		{Path: ".env.example", Status: "A", Patch: patchOf("@@ -0,0 +1 @@", "+DEBUG=1")},
		{Path: "deploy/id_ed25519.pub", Status: "A", Patch: patchOf("@@ -0,0 +1 @@", "+x")},
		{Path: "old.pem", Status: "D", Patch: patchOf("@@ -1 +0,0 @@", "-x")},
	}
	scan := ScanSecrets(ChangeSet{Files: files})
	got := []string{}
	for _, f := range scan.Findings {
		if f.Rule != "key-file" || f.Line != 0 {
			t.Fatalf("unexpected %+v", f)
		}
		got = append(got, f.Path)
	}
	if strings.Join(got, ",") != ".env,deploy/id_ed25519,certs/server.pem" {
		t.Fatalf("key files %v", got)
	}
}

func TestScanSecretsReportsWhatItCouldNotScanNeverAsClean(t *testing.T) {
	set := ChangeSet{
		Files: []ChangedFile{
			{Path: "img.png", Status: "A", Binary: true, Added: -1, Deleted: -1},
			{Path: "big.txt", Status: "A", Patch: patchOf("@@ -0,0 +1 @@", "+"+sampleAccessKey), PatchTruncated: true},
			{Path: "gone.txt", Status: "D", Patch: patchOf("@@ -1 +0,0 @@", "-x")},
		},
		Total:          5,
		FilesTruncated: true,
	}
	scan := ScanSecrets(set)
	reasons := map[string]string{}
	for _, n := range scan.NotScanned {
		reasons[n.Path] = n.Reason
	}
	if reasons["img.png"] != "binary" || reasons["big.txt"] != "too_large" || len(reasons) != 2 {
		t.Fatalf("not scanned %+v", scan.NotScanned)
	}
	if scan.Unlisted != 2 {
		t.Fatalf("unlisted %d", scan.Unlisted)
	}
	if len(scan.Findings) != 1 || scan.Findings[0].Path != "big.txt" {
		t.Fatalf("the scanned part of a cut patch is still reported: %+v", scan.Findings)
	}
	if scan.Complete() {
		t.Fatal("a scan with unscanned files reads complete")
	}
	if !ScanSecrets(ChangeSet{Files: []ChangedFile{}}).Complete() {
		t.Fatal("an empty change set reads incomplete")
	}
}

func TestScanSecretsGivesEachFindingADistinctID(t *testing.T) {
	patch := patchOf("@@ -0,0 +1,2 @@", "+"+sampleAccessKey+" "+sampleGitToken, "+"+sampleAccessKey)
	scan := ScanSecrets(ChangeSet{Files: []ChangedFile{{Path: "a", Status: "A", Patch: patch}}})
	if len(scan.Findings) != 3 {
		t.Fatalf("findings %+v", scan.Findings)
	}
	seen := map[string]bool{}
	for _, f := range scan.Findings {
		if seen[f.ID] {
			t.Fatalf("duplicate id %s", f.ID)
		}
		seen[f.ID] = true
	}
}

func TestScanSecretsStopsRecordingPastItsCapAndSaysSo(t *testing.T) {
	lines := []string{"@@ -0,0 +1," + strconv.Itoa(MaxSecretFindings+1) + " @@"}
	for range MaxSecretFindings + 1 {
		lines = append(lines, "+"+sampleAccessKey)
	}
	scan := ScanSecrets(ChangeSet{Files: []ChangedFile{{Path: "a", Status: "A", Patch: patchOf(lines...)}}})
	if len(scan.Findings) != MaxSecretFindings || !scan.FindingsCapped || scan.Complete() {
		t.Fatalf("findings %d capped %v", len(scan.Findings), scan.FindingsCapped)
	}
}
