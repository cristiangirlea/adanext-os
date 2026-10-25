package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	uuidPattern  = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	keyPattern   = regexp.MustCompile(`\b(sk|org|user|proj|acct)-[A-Za-z0-9_*-]+`)
	tokenPattern = regexp.MustCompile(`[A-Za-z0-9_+/=-]{32,}`)
)

// mask hides what identifies the owner in recorded output: email addresses, UUIDs, provider key
// and account prefixes, long tokens, the home directory and the user name.
func mask(text, home, user string) string {
	text = emailPattern.ReplaceAllString(text, "<email>")
	text = uuidPattern.ReplaceAllString(text, "<uuid>")
	text = keyPattern.ReplaceAllString(text, "$1-<masked>")
	text = tokenPattern.ReplaceAllString(text, "<id>")
	for _, p := range []string{home, strings.ReplaceAll(home, `\`, `\\`), strings.ReplaceAll(home, `\`, "/")} {
		if p != "" {
			text = strings.ReplaceAll(text, p, "~")
		}
	}
	if len(user) >= 3 {
		text = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(user)).ReplaceAllString(text, "<user>")
	}
	return text
}

// claudeSignIn accepts `claude auth status` only when it shows a claude.ai plan sign-in with the
// first-party provider, never an API key or a cloud provider that bills per token.
func claudeSignIn(status []byte) error {
	var s struct {
		LoggedIn         bool   `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		APIProvider      string `json:"apiProvider"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if err := json.Unmarshal(status, &s); err != nil {
		return errors.New("claude auth status printed no JSON")
	}
	switch {
	case !s.LoggedIn:
		return errors.New("claude is not signed in")
	case s.AuthMethod != "claude.ai":
		return fmt.Errorf("claude signs in with %q, not a claude.ai plan", s.AuthMethod)
	case s.APIProvider != "firstParty":
		return fmt.Errorf("claude uses the %q provider, not first-party", s.APIProvider)
	case s.SubscriptionType == "":
		return errors.New("claude reports no plan subscription")
	}
	return nil
}

// codexSignIn accepts `codex login status` only when it says exactly that ChatGPT is the sign-in.
func codexSignIn(status []byte) error {
	if strings.TrimSpace(string(status)) != "Logged in using ChatGPT" {
		return errors.New("codex is not signed in with a ChatGPT plan")
	}
	return nil
}

var refusedEnv = regexp.MustCompile(`(?i)(_API_KEY|_AUTH_TOKEN|_TOKEN)$|^ANTHROPIC_|^CLAUDE_CODE_USE_|^OPENAI_`)

// claudeSettings refuses a claude settings file holding an apiKeyHelper or a key, token or
// provider switch in env, and lists the plugins and hooks it turns on.
func claudeSettings(data []byte) ([]string, error) {
	var s struct {
		APIKeyHelper   any             `json:"apiKeyHelper"`
		Env            map[string]any  `json:"env"`
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
		Hooks          map[string]any  `json:"hooks"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, errors.New("the claude settings are not JSON")
	}
	if s.APIKeyHelper != nil {
		return nil, errors.New("the claude settings hold an apiKeyHelper")
	}
	for key := range s.Env {
		if refusedEnv.MatchString(key) {
			return nil, fmt.Errorf("the claude settings set %s in env", key)
		}
	}
	var found []string
	for plugin, on := range s.EnabledPlugins {
		if on {
			found = append(found, "plugin "+plugin)
		}
	}
	for event := range s.Hooks {
		found = append(found, "hooks "+event)
	}
	sort.Strings(found)
	return found, nil
}

// claudeMCPServers lists the user-level MCP servers in ~/.claude.json by name.
func claudeMCPServers(data []byte) []string {
	var s struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	var found []string
	for name := range s.MCPServers {
		found = append(found, "mcp server "+name)
	}
	sort.Strings(found)
	return found
}

var (
	// Matched anywhere on a line once quotes are gone: a table, a dotted or quoted key, an inline table.
	codexRefused  = regexp.MustCompile(`(?i)\b(model_providers|env_key|api_key|experimental_bearer_token|http_headers|env_http_headers)\b`)
	codexLogin    = regexp.MustCompile(`(?i)\b(preferred_auth_method|forced_login_method)\s*=\s*(\w*)`)
	codexProvider = regexp.MustCompile(`(?i)\bmodel_provider\s*=\s*([\w-]*)`)
	codexTable    = regexp.MustCompile(`^\s*\[\s*(mcp_servers|plugins)\.([A-Za-z0-9_-]+)\s*\]`)
)

// tomlScanner cuts TOML comments line by line, carrying an open multi-line string to the next line.
type tomlScanner struct{ quote string } // "", ", ', """ or '''

// uncomment cuts a TOML comment, a # outside a string, and refuses a unicode escape in a basic
// string, which could spell a refused key.
func (s *tomlScanner) uncomment(line string) (string, error) {
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case s.quote == "" && c == '#':
			return line[:i], nil
		case s.quote == "" && (c == '"' || c == '\''):
			s.quote = string(c)
			if triple := strings.Repeat(s.quote, 3); strings.HasPrefix(line[i:], triple) {
				s.quote = triple
				i += 2
			}
		case s.quote == "":
		case s.quote[0] == '"' && c == '\\':
			if i+1 < len(line) && (line[i+1] == 'u' || line[i+1] == 'U') {
				return "", errors.New("the codex config holds a unicode escape, which could spell a refused key")
			}
			i++
		case strings.HasPrefix(line[i:], s.quote):
			i += len(s.quote) - 1
			s.quote = ""
		}
	}
	if len(s.quote) == 1 {
		s.quote = "" // a one-line string ends with its line
	}
	return line, nil
}

// codexConfig refuses, failing closed, a codex config.toml that could sign in with or send an API
// key or use another provider: any model_providers, a model_provider other than openai, a key, token
// or header setting, an API sign-in, or a unicode escape that could spell one. It lists the MCP
// servers and plugins it declares. Comments are skipped.
func codexConfig(data []byte) ([]string, error) {
	var found []string
	var toml tomlScanner
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line, err := toml.uncomment(scanner.Text())
		if err != nil {
			return nil, err
		}
		bare := strings.NewReplacer(`"`, "", "'", "").Replace(line)
		if m := codexRefused.FindString(bare); m != "" {
			return nil, fmt.Errorf("the codex config holds %s", m)
		}
		if m := codexLogin.FindStringSubmatch(bare); m != nil && !strings.EqualFold(m[2], "chatgpt") {
			return nil, fmt.Errorf("the codex config sets %s to %q", m[1], m[2])
		}
		if m := codexProvider.FindStringSubmatch(bare); m != nil && m[1] != "openai" {
			return nil, fmt.Errorf("the codex config sets model_provider to %q, not openai", m[1])
		}
		if m := codexTable.FindStringSubmatch(line); m != nil {
			kind := "mcp server"
			if m[1] == "plugins" {
				kind = "plugin"
			}
			found = append(found, kind+" "+m[2])
		}
	}
	return found, scanner.Err()
}

// codexAuthHoldsAKey reports whether codex's auth.json stores an API key, without reading it out.
func codexAuthHoldsAKey(data []byte) bool {
	var auth map[string]any
	if json.Unmarshal(data, &auth) != nil {
		return false
	}
	key, ok := auth["OPENAI_API_KEY"]
	return ok && key != nil && key != ""
}

// claudeStatusRecord keeps only the plan fields of `claude auth status`; account fields such as
// email or orgName are never recorded.
func claudeStatusRecord(status []byte) map[string]any {
	out := map[string]any{}
	var s map[string]any
	if json.Unmarshal(status, &s) != nil {
		return out
	}
	for _, key := range []string{"loggedIn", "authMethod", "apiProvider", "subscriptionType"} {
		if v, ok := s[key]; ok {
			out[key] = v
		}
	}
	return out
}

// managedClaudeSettings are the files where an administrator's claude settings override the
// user's, on goos.
func managedClaudeSettings(goos string) []string {
	switch goos {
	case "windows":
		return []string{`C:\Program Files\ClaudeCode\managed-settings.json`, `C:\ProgramData\ClaudeCode\managed-settings.json`}
	case "darwin":
		return []string{"/Library/Application Support/ClaudeCode/managed-settings.json"}
	}
	return []string{"/etc/claude-code/managed-settings.json"}
}

// managedCodexConfigs are codex's system-wide config files on goos; Windows has none. The
// managed_config.toml in codex's home is read on every OS besides these.
func managedCodexConfigs(goos string) []string {
	if goos == "windows" {
		return nil
	}
	return []string{"/etc/codex/config.toml", "/etc/codex/managed_config.toml"}
}
