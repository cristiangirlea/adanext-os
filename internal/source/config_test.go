package source

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestConfigParsesGitsSyntax(t *testing.T) {
	t.Parallel()
	c, err := parseConfig([]byte("\xef\xbb\xbf# comment\n" +
		"[Core]\n" +
		"\tFileMode = false ; trailing comment\n" +
		"\tbare\n" +
		"\tautocrlf = \"in\\\"put\" # quoted\n" +
		"\teol = lf\\\n" +
		"  x\n" +
		"[remote \"Origin.Mixed\"]\n" +
		"\turl = https://example.com/a.git\n" +
		"[branch.Main] remote = origin\n" +
		"[include]\n\tpath = elsewhere\n"))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"core.filemode":           "false",
		"core.bare":               "true",
		"core.autocrlf":           `in"put`,
		"core.eol":                "lf  x",
		"remote.Origin.Mixed.url": "https://example.com/a.git",
		"branch.main.remote":      "origin",
		"include.path":            "elsewhere",
	} {
		if got, ok := c.get(key); !ok || got != want {
			t.Errorf("%s = %q (%v), want %q", key, got, ok, want)
		}
	}
	if c.boolean("core.filemode") || !c.boolean("core.bare") {
		t.Error("booleans read wrong")
	}
	if _, ok := c.get("remote.origin.mixed.url"); ok {
		t.Error("a quoted subsection matched case-insensitively")
	}
}

func TestConfigRefusesMalformedInput(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"key = outside\n", "[core\n", "[core]\n\tx = \"open\n", "[core]\n\tx = bad\\q\n", "[sec \"sub]\n"} {
		if _, err := parseConfig([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestTheLastValueWinsAcrossFiles(t *testing.T) {
	t.Parallel()
	a, _ := parseConfig([]byte("[core]\n\tautocrlf = true\n"))
	b, _ := parseConfig([]byte("[core]\n\tautocrlf = input\n"))
	a.merge(b)
	if v, _ := a.get("core.autocrlf"); v != "input" {
		t.Fatalf("autocrlf = %q", v)
	}
}

func TestSystemConfigIsFoundFromGitsPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		goos, git, programData string
		want                   []string
	}{
		{"linux", "/usr/bin/git", "", []string{"/etc/gitconfig"}},
		{"darwin", "/opt/homebrew/bin/git", "", []string{filepath.Join("/opt/homebrew", "etc", "gitconfig")}},
		{"windows", `C:\Program Files\Git\cmd\git.exe`, `C:\ProgramData`, []string{filepath.Join(`C:\ProgramData`, "Git", "config"), filepath.Join(`C:\Program Files\Git`, "etc", "gitconfig")}},
		{"windows", `C:\Program Files\Git\mingw64\bin\git.exe`, "", []string{filepath.Join(`C:\Program Files\Git`, "etc", "gitconfig")}},
		{"windows", `C:\Program Files\Git\bin\git.exe`, "", []string{filepath.Join(`C:\Program Files\Git`, "etc", "gitconfig")}},
	}
	for _, c := range cases {
		if c.goos == "windows" && filepath.Separator != '\\' || c.goos != "windows" && filepath.Separator != '/' {
			continue // paths of the other platform do not split here
		}
		if got := systemConfigFiles(c.goos, c.git, c.programData); !slices.Equal(got, c.want) {
			t.Errorf("%s %s: %v, want %v", c.goos, c.git, got, c.want)
		}
	}
}

func TestDefaultConfigFilesFollowTheEnvironment(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GIT_CONFIG_SYSTEM", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	cf := DefaultConfigFiles("/usr/bin/git")
	if len(cf.System) != 0 {
		t.Errorf("system = %v under GIT_CONFIG_NOSYSTEM", cf.System)
	}
	xdg := filepath.Join(home, ".config", "git")
	if cf.XDG != xdg || cf.Home != home || !slices.Equal(cf.Global, []string{filepath.Join(xdg, "config"), filepath.Join(home, ".gitconfig")}) {
		t.Errorf("config files = %+v", cf)
	}
	if got := cf.userFile(&config{}, "core.excludesFile", "ignore"); got != filepath.Join(xdg, "ignore") {
		t.Errorf("default excludes = %q", got)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "only"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(home, "system"))
	cf = DefaultConfigFiles("/usr/bin/git")
	if !slices.Equal(cf.Global, []string{filepath.Join(home, "only")}) || !slices.Equal(cf.System, []string{filepath.Join(home, "system")}) {
		t.Errorf("overridden config files = %+v", cf)
	}
}
