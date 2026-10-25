package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTheMatrixRefusesEachUnsupportedCombination(t *testing.T) {
	m := Capabilities()
	refused := map[string]StartOptions{
		"an unknown backend":           {Backend: "gemini", Mode: "readonly"},
		"the api backend":              {Backend: "api", Mode: "readonly"},
		"claude in mode ask":           {Backend: "claude", Mode: "ask"},
		"codex in mode ask":            {Backend: "codex", Mode: "ask"},
		"an unknown mode":              {Backend: "codex", Mode: "plan"},
		"an effort codex cannot apply": {Backend: "codex", Mode: "auto", Effort: "turbo"},
		"an effort claude cannot take": {Backend: "claude", Mode: "auto", Effort: "minimal"},
		"a turn limit on codex":        {Backend: "codex", Mode: "readonly", MaxTurns: 3},
	}
	for name, o := range refused {
		t.Run(name, func(t *testing.T) {
			err := m.Check(o)
			var unsupported *UnsupportedError
			if !errors.As(err, &unsupported) || unsupported.Reason == "" {
				t.Fatalf("Check(%+v) = %v, want an UnsupportedError with a reason", o, err)
			}
		})
	}
	for _, o := range []StartOptions{
		{Backend: "claude", Mode: "readonly", MaxTurns: 3, Timeout: 3 * time.Minute},
		{Backend: "claude", Mode: "yolo", Model: "claude-opus-5-5", Effort: "max"},
		{Backend: "codex", Mode: "auto", Model: "gpt-6.1-sol", Effort: "high", Timeout: time.Minute},
		{Backend: "codex", Mode: "readonly"},
	} {
		if err := m.Check(o); err != nil {
			t.Errorf("Check(%+v) = %v", o, err)
		}
	}
}

func TestEveryOfferedModeHasEveryRowAndEveryCellSaysWhoHoldsIt(t *testing.T) {
	m := Capabilities()
	if m.Checks.Status != Unsandboxed || m.Checks.Detail == "" {
		t.Fatalf("checks = %+v, want unsandboxed with a reason", m.Checks)
	}
	if !fullRevision.MatchString(m.Engine) {
		t.Fatalf("engine revision %q", m.Engine)
	}
	for _, b := range m.Backends {
		if b.CLIVersion == "" || b.Observed == "" {
			t.Errorf("%s: no observed CLI version or date", b.Backend)
		}
		var modes []string
		for _, mode := range b.Modes {
			modes = append(modes, mode.Mode)
			if !mode.Offered {
				if mode.Reason == "" {
					t.Errorf("%s %s: refused without a reason", b.Backend, mode.Mode)
				}
				continue
			}
			for _, row := range Rows {
				cell, ok := mode.Cells[row]
				switch {
				case !ok:
					t.Errorf("%s %s: no %s cell", b.Backend, mode.Mode, row)
				case cell.Status != Enforced && cell.Status != Requested && cell.Status != Unsupported:
					t.Errorf("%s %s %s: status %q", b.Backend, mode.Mode, row, cell.Status)
				case cell.Detail == "":
					t.Errorf("%s %s %s: no detail", b.Backend, mode.Mode, row)
				case cell.Status == Enforced && (cell.By == "" || cell.Test == ""):
					t.Errorf("%s %s %s: enforced without who and which test", b.Backend, mode.Mode, row)
				case cell.Status == Requested && cell.By == "":
					t.Errorf("%s %s %s: requested of no one", b.Backend, mode.Mode, row)
				}
			}
			if len(mode.Cells) != len(Rows) {
				t.Errorf("%s %s: %d cells for %d rows", b.Backend, mode.Mode, len(mode.Cells), len(Rows))
			}
		}
		if !slices.Equal(modes, []string{"readonly", "auto", "yolo", "ask"}) {
			t.Errorf("%s: modes %v", b.Backend, modes)
		}
	}
}

func TestTheMatrixIsWhatThePinnedEngineAdvertises(t *testing.T) {
	// What hrn exec's hello says at the pinned revision (internal/agent's claude.go and codex.go).
	want := map[string][]string{
		"claude": {"model", "mode", "effort", "timeout", "max_turns", "mcp"},
		"codex":  {"model", "mode", "effort", "timeout"},
	}
	m := Capabilities()
	if len(m.Backends) != len(want) {
		t.Fatalf("backends %d", len(m.Backends))
	}
	for _, b := range m.Backends {
		if !slices.Equal(b.Options, want[b.Backend]) {
			t.Errorf("%s options %v, want %v", b.Backend, b.Options, want[b.Backend])
		}
		if !slices.Equal(b.Efforts, []string{"low", "medium", "high", "xhigh", "max"}) {
			t.Errorf("%s efforts %v", b.Backend, b.Efforts)
		}
	}
}

func TestACellForAnotherCLIVersionShowsUnverified(t *testing.T) {
	m := Capabilities()
	codex := m.backend("codex")
	same := m.WithInstalled(map[string]Installed{
		"codex":  {Available: true, Version: codex.CLIVersion, Modes: []string{"readonly", "auto", "yolo"}, Options: codex.Options},
		"claude": {Available: true, Version: "0.0.1"},
	})
	if got := same.backend("codex").cell("readonly", RowReadOnly); got.Status != Requested || got.Recorded != "" {
		t.Fatalf("same version: %+v", got)
	}
	claude := same.backend("claude")
	if got := claude.cell("readonly", RowReadOnly); got.Status != Unverified || got.Recorded != Requested {
		t.Fatalf("other version: read-only cell %+v", got)
	}
	if got, want := claude.cell("readonly", RowCancellation), m.backend("claude").cell("readonly", RowCancellation); got.Status != want.Status {
		t.Fatalf("other version: adad's cancellation %+v, want it unchanged at %s", got, want.Status)
	}
	if claude.Installed == nil || claude.Installed.Version != "0.0.1" {
		t.Fatalf("installed %+v", claude.Installed)
	}
	missing := m.WithInstalled(nil)
	if got := missing.backend("codex").cell("auto", RowWorkspaceWrites); got.Status != Unverified || got.Recorded != Requested {
		t.Fatalf("no CLI found: %+v", got)
	}
	// The engine advertising other options than the table is a different engine.
	other := m.WithInstalled(map[string]Installed{"codex": {Available: true, Version: codex.CLIVersion, Modes: []string{"readonly"}, Options: codex.Options}})
	if got := other.backend("codex").cell("readonly", RowReadOnly); got.Status != Unverified {
		t.Fatalf("other engine answers: %+v", got)
	}
	if m.backend("claude").cell("readonly", RowReadOnly).Status != Requested {
		t.Fatal("WithInstalled changed the static table")
	}
}

func TestParseCLIVersionTakesTheVersionFromTheCLIsOwnOutput(t *testing.T) {
	for output, want := range map[string]string{
		"2.1.284 (Claude Code)\n":        "2.1.284",
		"codex-cli 0.163.0-alpha.11\r\n": "0.163.0-alpha.11",
		"no version here":                "",
		"C:\\Users\\someone\\1.2.3 path": "1.2.3",
	} {
		if got := ParseCLIVersion(output); got != want {
			t.Errorf("ParseCLIVersion(%q) = %q, want %q", output, got, want)
		}
	}
	if got := ParseCLIVersion(strings.Repeat("9", 100) + ".1.1"); len(got) > 64 {
		t.Errorf("an overlong version was kept: %q", got)
	}
}

func TestCancellationIsEnforcedOnlyWhereTheWholeTreeIsKilled(t *testing.T) {
	for goos, want := range map[string]Status{"windows": Enforced, "linux": Enforced, "darwin": Requested} {
		m := capabilitiesFor(goos)
		for _, b := range m.Backends {
			for _, mode := range []string{"readonly", "auto", "yolo"} {
				if got := b.cell(mode, RowCancellation); got.Status != want {
					t.Errorf("%s %s %s: cancellation %s, want %s", goos, b.Backend, mode, got.Status, want)
				}
			}
		}
	}
}
