package source

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type configEntry struct {
	section, subsection, name string // section and name lower case; subsection as written
	value                     string
	hasValue                  bool
}

// config is git config as a list of entries in reading order; the last
// value of a key wins. include and includeIf are plain entries here, never
// followed.
type config struct{ entries []configEntry }

func splitKey(key string) (section, subsection, name string) {
	first, last := strings.Index(key, "."), strings.LastIndex(key, ".")
	section, name = strings.ToLower(key[:first]), strings.ToLower(key[last+1:])
	if first != last {
		subsection = key[first+1 : last]
	}
	return
}

func (c *config) lookup(key string) (configEntry, bool) {
	section, subsection, name := splitKey(key)
	for i := len(c.entries) - 1; i >= 0; i-- {
		e := c.entries[i]
		if e.section == section && e.subsection == subsection && e.name == name {
			return e, true
		}
	}
	return configEntry{}, false
}

func (c *config) get(key string) (string, bool) {
	e, ok := c.lookup(key)
	if ok && !e.hasValue {
		return "true", true
	}
	return e.value, ok
}

// boolean reads a key as git does; unset or unparsable is false.
func (c *config) boolean(key string) bool {
	e, ok := c.lookup(key)
	if !ok {
		return false
	}
	return !e.hasValue || parseBool(e.value)
}

func parseBool(v string) bool {
	switch strings.ToLower(v) {
	case "true", "yes", "on":
		return true
	case "false", "no", "off", "":
		return false
	}
	n, err := strconv.Atoi(v)
	return err == nil && n != 0
}

func (c *config) merge(other *config) { c.entries = append(c.entries, other.entries...) }

// readConfigFile parses a config file under base, read through no link; with
// base empty it is the user's own file, whose local links are followed. A
// missing file is an empty config.
func readConfigFile(base, path string) (*config, error) {
	var data []byte
	var err error
	if base == "" {
		data, err = readUserFile(path)
	} else {
		data, _, err = readUnder(base, path, maxFileSize)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return &config{}, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := parseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func isKeyChar(b byte) bool {
	return b == '-' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func parseConfig(data []byte) (*config, error) {
	s := string(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}))
	c := &config{}
	var section, subsection string
	i, line := 0, 1
	fail := func(what string) (*config, error) { return nil, fmt.Errorf("line %d: %s", line, what) }
	for i < len(s) {
		ch := s[i]
		switch {
		case ch == '\n':
			line++
			i++
		case ch == ' ' || ch == '\t' || ch == '\r':
			i++
		case ch == '#' || ch == ';':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case ch == '[':
			i++
			start := i
			for i < len(s) && (isKeyChar(s[i]) || s[i] == '.') {
				i++
			}
			name := s[start:i]
			if name == "" {
				return fail("bad section header")
			}
			subsection = ""
			if i < len(s) && (s[i] == ' ' || s[i] == '\t') {
				for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
					i++
				}
				if i >= len(s) || s[i] != '"' {
					return fail("bad section header")
				}
				i++
				var sub strings.Builder
				for i < len(s) && s[i] != '"' {
					if s[i] == '\n' {
						return fail("newline in subsection")
					}
					if s[i] == '\\' && i+1 < len(s) {
						i++
					}
					sub.WriteByte(s[i])
					i++
				}
				if i >= len(s) {
					return fail("unterminated subsection")
				}
				i++
				subsection = sub.String()
			} else if dot := strings.Index(name, "."); dot >= 0 {
				// The legacy [section.subsection] form.
				name, subsection = name[:dot], strings.ToLower(name[dot+1:])
			}
			if i >= len(s) || s[i] != ']' {
				return fail("bad section header")
			}
			i++
			section = strings.ToLower(name)
		case isKeyChar(ch):
			if section == "" {
				return fail("key outside a section")
			}
			start := i
			for i < len(s) && isKeyChar(s[i]) {
				i++
			}
			e := configEntry{section: section, subsection: subsection, name: strings.ToLower(s[start:i])}
			for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
				i++
			}
			if i < len(s) && s[i] == '=' {
				i++
				var err error
				e.value, i, err = parseValue(s, i, &line)
				if err != nil {
					return fail(err.Error())
				}
				e.hasValue = true
			} else if i < len(s) && s[i] != '\n' && s[i] != '\r' && s[i] != '#' && s[i] != ';' {
				return fail("bad key")
			}
			c.entries = append(c.entries, e)
		default:
			return fail("unexpected character")
		}
	}
	return c, nil
}

// parseValue reads a value up to the end of its line, as git does: quotes,
// escapes and continuation lines; comments and outer spaces dropped.
func parseValue(s string, i int, line *int) (string, int, error) {
	var b strings.Builder
	quoted := false
	spaces := 0
	for ; i < len(s); i++ {
		ch := s[i]
		if ch == '\n' {
			if quoted {
				return "", i, errors.New("newline in quoted value")
			}
			break
		}
		if !quoted && (ch == ' ' || ch == '\t' || ch == '\r') {
			if b.Len() > 0 {
				spaces++
			}
			continue
		}
		if !quoted && (ch == '#' || ch == ';') {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			break
		}
		for ; spaces > 0; spaces-- {
			b.WriteByte(' ')
		}
		switch ch {
		case '"':
			quoted = !quoted
		case '\\':
			i++
			if i >= len(s) {
				return "", i, errors.New("bad escape")
			}
			switch s[i] {
			case '\n':
				*line++
			case '\r':
				if i+1 < len(s) && s[i+1] == '\n' {
					i++
					*line++
				}
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case '"', '\\':
				b.WriteByte(s[i])
			default:
				return "", i, errors.New("bad escape")
			}
		default:
			b.WriteByte(ch)
		}
	}
	if quoted {
		return "", i, errors.New("unterminated quote")
	}
	return b.String(), i, nil
}

// ConfigFiles are the user's system and global config files, read in order,
// and the directories git uses for defaults, as the user's own git finds them.
type ConfigFiles struct {
	System []string
	Global []string
	XDG    string // the git directory under XDG_CONFIG_HOME or ~/.config
	Home   string // for "~/" in core.excludesFile and core.attributesFile
}

// DefaultConfigFiles resolves the user's config files from Ada's environment
// and the resolved git binary.
func DefaultConfigFiles(gitPath string) ConfigFiles {
	home := os.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	cf := ConfigFiles{Home: home}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		cf.XDG = filepath.Join(xdg, "git")
	} else if home != "" {
		cf.XDG = filepath.Join(home, ".config", "git")
	}
	if global, ok := os.LookupEnv("GIT_CONFIG_GLOBAL"); ok {
		if global != "" && global != os.DevNull {
			cf.Global = []string{global}
		}
	} else {
		if cf.XDG != "" {
			cf.Global = append(cf.Global, filepath.Join(cf.XDG, "config"))
		}
		if home != "" {
			cf.Global = append(cf.Global, filepath.Join(home, ".gitconfig"))
		}
	}
	switch system, ok := os.LookupEnv("GIT_CONFIG_SYSTEM"); {
	case parseBool(os.Getenv("GIT_CONFIG_NOSYSTEM")):
	case ok:
		if system != "" && system != os.DevNull {
			cf.System = []string{system}
		}
	default:
		cf.System = systemConfigFiles(runtime.GOOS, gitPath, os.Getenv("PROGRAMDATA"))
	}
	return cf
}

// systemConfigFiles finds the system config from git's install prefix; Git
// for Windows also reads %PROGRAMDATA%\Git\config first.
func systemConfigFiles(goos, gitPath, programData string) []string {
	if goos != "windows" {
		prefix := filepath.Dir(filepath.Dir(gitPath))
		if prefix == "/usr" || prefix == "/" {
			return []string{"/etc/gitconfig"}
		}
		return []string{filepath.Join(prefix, "etc", "gitconfig")}
	}
	var files []string
	if programData != "" {
		files = append(files, filepath.Join(programData, "Git", "config"))
	}
	prefix := filepath.Dir(gitPath)
	if base := strings.ToLower(filepath.Base(prefix)); base == "cmd" || base == "bin" {
		prefix = filepath.Dir(prefix)
		switch strings.ToLower(filepath.Base(prefix)) {
		case "mingw64", "mingw32", "clangarm64", "usr":
			prefix = filepath.Dir(prefix)
		}
	}
	return append(files, filepath.Join(prefix, "etc", "gitconfig"))
}

// userFile resolves core.excludesFile or core.attributesFile, or git's
// default under XDG when the key is unset.
func (cf ConfigFiles) userFile(c *config, key, fallback string) string {
	v, ok := c.get(key)
	if !ok {
		if cf.XDG == "" {
			return ""
		}
		return filepath.Join(cf.XDG, fallback)
	}
	if rest, ok := strings.CutPrefix(v, "~/"); ok && cf.Home != "" {
		return filepath.Join(cf.Home, filepath.FromSlash(rest))
	}
	if !filepath.IsAbs(v) {
		return ""
	}
	return v
}
