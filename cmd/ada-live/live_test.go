package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refusingRunner fails the test if the harness runs anything.
type refusingRunner struct{ t *testing.T }

func (r refusingRunner) Output(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
	r.t.Fatalf("the harness ran %s %v without the opt-in", name, args)
	return nil, nil
}

func TestTheHarnessRefusesWithoutTheOptIn(t *testing.T) {
	for _, value := range []string{"", "0", "true", "yes", " 1", "1 "} {
		state := filepath.Join(t.TempDir(), "state")
		var out bytes.Buffer
		env := func(key string) string {
			if key == "ADANEXT_LIVE" {
				return value
			}
			return ""
		}
		code := run(context.Background(), []string{"-backend", "claude", "-state-dir", state, "-engine", "hrn", "-adad", "adad"}, env, refusingRunner{t}, &out)
		if code != 2 || !strings.Contains(out.String(), "ADANEXT_LIVE=1") {
			t.Fatalf("ADANEXT_LIVE=%q: exit %d: %s", value, code, out.String())
		}
		if _, err := os.Stat(state); err == nil {
			t.Fatalf("ADANEXT_LIVE=%q: the state directory was created", value)
		}
	}
}

func TestTheHarnessNeedsTheBackendNamed(t *testing.T) {
	env := func(key string) string {
		if key == "ADANEXT_LIVE" {
			return "1"
		}
		return ""
	}
	for _, args := range [][]string{
		{"-state-dir", t.TempDir(), "-engine", "hrn", "-adad", "adad"},
		{"-backend", "api", "-state-dir", t.TempDir(), "-engine", "hrn", "-adad", "adad"},
		{"-backend", "claude", "-state-dir", t.TempDir(), "-engine", "hrn", "-adad", "adad", "-timeout", "1h"},
		{"-backend", "claude", "-state-dir", t.TempDir(), "-engine", "hrn", "-adad", "adad", "-max-turns", "50"},
	} {
		var out bytes.Buffer
		if code := run(context.Background(), args, env, refusingRunner{t}, &out); code != 2 {
			t.Fatalf("%v: exit %d: %s", args, code, out.String())
		}
	}
}

func TestCINeverSetsTheLiveOptIn(t *testing.T) {
	workflows, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*"))
	if err != nil || len(workflows) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	for _, path := range append(workflows, filepath.Join("..", "..", "scripts", "check.sh")) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("ADANEXT_LIVE")) {
			t.Errorf("%s mentions ADANEXT_LIVE", path)
		}
	}
}

func TestMaskHidesIdentifiers(t *testing.T) {
	in := `{"email": "someone@example.com", "orgId": "0f8fad5b-d9cb-469f-a165-70867728950e", "orgName": "someone@example.com's Organization",
"projectsDirectory": "C:\\Users\\someone\\.claude\\projects", "key": "sk-ant-abcdefghijklmnop", "token": "abcdefghijklmnopqrstuvwxyz0123456789ABCD"}`
	got := mask(in, `C:\Users\someone`, "someone")
	for _, leak := range []string{"someone", "0f8fad5b", "sk-ant-abc", "abcdefghijklmnopqrstuvwxyz0123456789ABCD"} {
		if strings.Contains(got, leak) {
			t.Errorf("mask left %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, `"loggedIn"`) && !strings.Contains(got, "<email>") {
		t.Errorf("mask removed the structure: %s", got)
	}
}

func TestClaudesStatusMustShowAPlanSignIn(t *testing.T) {
	for status, ok := range map[string]bool{
		`{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty", "subscriptionType": "max"}`: true,
		`{"loggedIn": true, "authMethod": "api_key", "apiProvider": "firstParty"}`:                              false,
		`{"loggedIn": false, "authMethod": "claude.ai", "apiProvider": "firstParty"}`:                           false,
		`{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "bedrock", "subscriptionType": "max"}`:    false,
		`{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty"}`:                            false,
		`not json`: false,
	} {
		if err := claudeSignIn([]byte(status)); (err == nil) != ok {
			t.Errorf("%s: %v", status, err)
		}
	}
}

func TestCodexsStatusMustShowAPlanSignIn(t *testing.T) {
	for status, ok := range map[string]bool{
		"Logged in using ChatGPT\n":                  true,
		"Logged in using an API key - sk-proj-***\n": false,
		"Not logged in\n":                            false,
		"Logged in using ChatGPT\nand an API key\n":  false,
	} {
		if err := codexSignIn([]byte(status)); (err == nil) != ok {
			t.Errorf("%q: %v", status, err)
		}
	}
}

func TestClaudeSettingsWithAKeyHelperOrAKeyInEnvAreRefused(t *testing.T) {
	for settings, ok := range map[string]bool{
		`{"permissions": {}, "env": {"DISABLE_TELEMETRY": "1"}, "enabledPlugins": {"a@b": true}}`: true,
		`{"apiKeyHelper": "/bin/get-key"}`:                 false,
		`{"env": {"ANTHROPIC_API_KEY": "x"}}`:              false,
		`{"env": {"ANTHROPIC_AUTH_TOKEN": "x"}}`:           false,
		`{"env": {"OPENAI_API_KEY": "x"}}`:                 false,
		`{"env": {"CLAUDE_CODE_USE_BEDROCK": "1"}}`:        false,
		`{"env": {"ANTHROPIC_BASE_URL": "https://proxy"}}`: false,
		`not json`: false,
	} {
		if _, err := claudeSettings([]byte(settings)); (err == nil) != ok {
			t.Errorf("%s: %v", settings, err)
		}
	}
	found, err := claudeSettings([]byte(`{"enabledPlugins": {"tool@market": true}, "hooks": {"Stop": []}}`))
	if err != nil || !strings.Contains(strings.Join(found, ","), "tool@market") || !strings.Contains(strings.Join(found, ","), "hooks") {
		t.Fatalf("found = %v, %v", found, err)
	}
}

func TestCodexConfigWithAnAPIKeyIsRefused(t *testing.T) {
	for config, ok := range map[string]bool{
		"model = \"gpt-6.1-sol\"\n[mcp_servers.docs]\ncommand = \"docs\"\n": true,
		"# env_key = \"OPENAI_API_KEY\" is only a comment\n":                true,
		"[model_providers.x]\nenv_key = \"OPENAI_API_KEY\"\n":               false,
		"experimental_bearer_token = \"abc\"\n":                             false,
		"preferred_auth_method = \"apikey\"\n":                              false,
		"forced_login_method = \"api\"\n":                                   false,
		// Every other route to a key or another provider is refused too.
		"model_provider = \"openai\"\n":                                      true,
		"model_providers.x.env_key = \"K\"\n":                                false,
		"[model_providers.x]\nbase_url = \"https://proxy\"\n":                false,
		"model_providers = { x = { name = \"x\" } }\n":                       false,
		"[ model_providers . x ]\n":                                          false,
		"model_provider = \"azure\"\n":                                       false,
		"[profiles.p]\nmodel_provider = 'x'\n":                               false,
		"profiles.p.model_provider = \"x\"\n":                                false,
		"x = { \"api_key\" = \"k\" }\n":                                      false,
		"x = { a = \"#\", api_key = \"k\" }\n":                               false,
		"'env_key' = 'K'\n":                                                  false,
		"\"api\\u005fkey\" = \"k\"\n":                                        false,
		"preferred_auth_method = 'apikey'\n":                                 false,
		"forced_login_method = 'api'\n":                                      false,
		"[mcp_servers.s]\nhttp_headers = { Authorization = \"Bearer k\" }\n": false,
		"[mcp_servers.s]\nenv_http_headers = { \"X\" = \"Y\" }\n":            false,
		"[mcp_servers.s.http_headers]\nAuthorization = \"Bearer k\"\n":       false,
	} {
		if _, err := codexConfig([]byte(config)); (err == nil) != ok {
			t.Errorf("%q: %v", config, err)
		}
	}
	// A server's sub-table is not another server.
	found, _ := codexConfig([]byte("[mcp_servers.docs]\ncommand = \"docs\"\n[mcp_servers.docs.env]\nA = \"b\"\n"))
	if strings.Join(found, ",") != "mcp server docs" {
		t.Fatalf("found = %v", found)
	}
}

func TestTheFixtureHoldsAProjectHookThatWritesAMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fixture")
	if err := writeFixture(dir, "windows"); err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil || !bytes.Contains(settings, []byte(hookMarker)) || !bytes.Contains(settings, []byte("SessionStart")) {
		t.Fatalf("settings = %s, %v", settings, err)
	}
	config, err := os.ReadFile(filepath.Join(dir, ".codex", "config.toml"))
	if err != nil || !bytes.Contains(config, []byte(codexMarker)) {
		t.Fatalf("codex config = %s, %v", config, err)
	}
}

func TestOnlyThePlanFieldsOfClaudesStatusAreRecorded(t *testing.T) {
	status := `{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty", "subscriptionType": "max", "email": "a@b.c", "orgName": "Some Person's Organization", "orgId": "x"}`
	got := claudeStatusRecord([]byte(status))
	want := map[string]any{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty", "subscriptionType": "max"}
	if len(got) != len(want) {
		t.Fatalf("recorded %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if got := claudeStatusRecord([]byte("not json")); len(got) != 0 {
		t.Fatalf("unparsed status recorded %v", got)
	}
}

// planRunner answers the sign-in commands as a plan sign-in does.
type planRunner struct{}

func (planRunner) Output(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	switch strings.Join(args, " ") {
	case "auth status":
		return []byte(`{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty", "subscriptionType": "max"}`), nil
	case "login status":
		return []byte("Logged in using ChatGPT\n"), nil
	}
	return []byte("1.0.0\n"), nil
}

func TestManagedSettingsGoThroughTheSameChecks(t *testing.T) {
	dir := t.TempDir()
	c := &config{home: dir, claudeConfig: filepath.Join(dir, "claude"), codexHome: filepath.Join(dir, "codex"),
		claudeManaged: []string{filepath.Join(dir, "absent.json")}, codexManaged: []string{filepath.Join(dir, "absent.toml")}}
	for _, backend := range []string{"claude", "codex"} {
		if err := c.preflight(context.Background(), planRunner{}, backend, "cli", map[string]any{}); err != nil {
			t.Fatalf("%s with no managed settings: %v", backend, err)
		}
	}
	managed := filepath.Join(dir, "managed-settings.json")
	if err := os.WriteFile(managed, []byte(`{"apiKeyHelper": "/bin/key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.claudeManaged = []string{managed}
	if err := c.preflight(context.Background(), planRunner{}, "claude", "cli", map[string]any{}); err == nil || !strings.Contains(err.Error(), "apiKeyHelper") {
		t.Fatalf("claude managed settings with a key helper: %v", err)
	}
	system := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(system, []byte("[model_providers.x]\nenv_key = \"K\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.codexManaged = []string{system}
	if err := c.preflight(context.Background(), planRunner{}, "codex", "cli", map[string]any{}); err == nil {
		t.Fatal("codex's system config with another provider was accepted")
	}
	for _, goos := range []string{"windows", "linux", "darwin"} {
		if len(managedClaudeSettings(goos)) == 0 {
			t.Errorf("%s: no managed claude settings path", goos)
		}
	}
	if len(managedCodexConfigs("linux")) == 0 || len(managedCodexConfigs("darwin")) == 0 {
		t.Error("no codex system config path")
	}
}
