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

var codexRefused = []*regexp.Regexp{
	regexp.MustCompile(`^\s*env_key\s*=`),
	regexp.MustCompile(`^\s*api_key\s*=`),
	regexp.MustCompile(`^\s*experimental_bearer_token\s*=`),
	regexp.MustCompile(`^\s*preferred_auth_method\s*=\s*"apikey"`),
	regexp.MustCompile(`^\s*forced_login_method\s*=\s*"api"`),
}

var codexTable = regexp.MustCompile(`^\s*\[\s*(mcp_servers|plugins)\.([A-Za-z0-9_-]+)\s*\]`)

// codexConfig refuses a codex config.toml that signs in with or sends an API key, and lists the
// MCP servers and plugins it declares. Comments are skipped.
func codexConfig(data []byte) ([]string, error) {
	var found []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line, _, _ := strings.Cut(scanner.Text(), "#")
		for _, refused := range codexRefused {
			if refused.MatchString(line) {
				return nil, fmt.Errorf("the codex config holds %s", strings.TrimSpace(strings.SplitN(line, "=", 2)[0]))
			}
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
