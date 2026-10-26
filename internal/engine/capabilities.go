package engine

import (
	"fmt"
	"maps"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Status is what a capability cell claims (decisions log, 2026-10-20 and 2026-10-25).
type Status string

const (
	Enforced    Status = "enforced"    // held by the named party; Test is what shows it
	Requested   Status = "requested"   // passed to the CLI, not verified
	Unsupported Status = "unsupported" // not offered
	Unverified  Status = "unverified"  // recorded for another CLI version, or none was found
	Unsandboxed Status = "unsandboxed" // runs as the user on the host (checks, M4.2)
)

// The rows of the matrix.
const (
	RowReadOnly        = "read_only"
	RowWorkspaceWrites = "workspace_writes_and_shell"
	RowNetwork         = "network"
	RowApprovals       = "approvals"
	RowContinuation    = "continuation"
	RowCancellation    = "cancellation"
	RowUsage           = "usage_reporting"
	RowModelDiscovery  = "model_discovery"
	RowTimeLimit       = "time_limit"
	RowTurnLimit       = "turn_limit"
)

// Rows lists every row an offered mode has, in display order.
var Rows = []string{RowReadOnly, RowWorkspaceWrites, RowNetwork, RowApprovals, RowContinuation, RowCancellation, RowUsage, RowModelDiscovery, RowTimeLimit, RowTurnLimit}

// Cell is one backend, mode and row.
type Cell struct {
	Status Status `json:"status"`
	By     string `json:"by,omitempty"`   // who enforces it, or who is asked
	Test   string `json:"test,omitempty"` // the test that shows it, or will
	Detail string `json:"detail"`
	// Recorded is the table's status when the cell is shown unverified.
	Recorded Status `json:"recorded,omitempty"`
	// cli marks a cell that depends on the CLI and so on its version.
	cli bool
}

// Mode is one mode of a backend; Ask is listed and refused.
type Mode struct {
	Mode    string          `json:"mode"`
	Offered bool            `json:"offered"`
	Reason  string          `json:"reason,omitempty"`
	Cells   map[string]Cell `json:"cells,omitempty"`
}

// Installed is what adad found of a backend's CLI on this host, through the engine's hello and
// the CLI's own version command. Modes and Options are the engine's answers; nothing here is a
// path or a secret.
type Installed struct {
	Available bool     `json:"available"`
	Version   string   `json:"version"`
	Reason    string   `json:"reason,omitempty"`
	Modes     []string `json:"-"`
	Options   []string `json:"-"`
}

// Backend is one backend's part of the matrix.
type Backend struct {
	Backend    string `json:"backend"`
	CLIVersion string `json:"cli_version"` // the version the cells were recorded against
	Observed   string `json:"observed"`    // when that version was observed
	// Installed is filled in by WithInstalled.
	Installed *Installed `json:"installed,omitempty"`
	Efforts   []string   `json:"efforts"`
	Options   []string   `json:"options"`
	// Models are those that have run through hrn on the owner's plans; the engine discovers none.
	Models []string `json:"models"`
	Modes  []Mode   `json:"modes"`
}

// Matrix is the capability matrix of the pinned engine.
type Matrix struct {
	Engine   string    `json:"engine"`
	Recorded string    `json:"recorded"`
	Checks   Cell      `json:"checks"`
	Backends []Backend `json:"backends"`
}

// StartOptions are what a start asks of a backend.
type StartOptions struct {
	Backend, Mode, Model, Effort string
	MaxTurns                     int
	Timeout                      time.Duration
}

// UnsupportedError is a start the matrix refuses.
type UnsupportedError struct{ Reason string }

func (e *UnsupportedError) Error() string { return "unsupported options: " + e.Reason }

const (
	pinnedRevision = "8dd2659b953e9e5d9c593fa655254167ea9f8a8c"
	recorded       = "2026-10-25"
	liveReadonly   = "cmd/ada-live readonly run (write asked, file absent)"
	liveDenied     = "cmd/ada-live readonly run (write asked, access denied)"
)

var efforts = []string{"low", "medium", "high", "xhigh", "max"}

// cancellation is adad's stop of the engine's tree on goos: enforced where the whole tree is
// killed, only requested on macOS, where only the engine's group is.
func cancellation(goos string) Cell {
	if goos == "darwin" {
		return Cell{Status: Requested, By: "adad: hrn's cancel, then the engine's process group",
			Detail: "on macOS only the engine's group is killed while it runs; a descendant that left it can outlive the cancel"}
	}
	return Cell{Status: Enforced, By: "adad: hrn's cancel, then the engine's whole process tree (a kill-on-close Job Object on Windows, the session's groups on Linux)",
		Test:   "internal/executor TestCancelWaitsForEngineExitAndKillsAnIgnoringEngine, TestForceKillStopsNestedBackendAndGrandchild",
		Detail: "the CLI's own cooperation is not needed"}
}

// common are the rows a backend's modes share; cli marks the CLI-dependent ones.
func common(goos string, usageBy, turns Cell) map[string]Cell {
	return map[string]Cell{
		RowApprovals: {Status: Unsupported, By: "hrn exec protocol v1",
			Detail: "the pinned protocol advertises approvals: false and refuses mode ask; nothing answers a CLI's prompt (#7)"},
		RowContinuation: {Status: Unsupported, By: "adad",
			Detail: "hrn can continue a run (sessions: fork), but every attempt is a fresh session until M6; refine is a new session (decisions log, 2026-10-20)"},
		RowCancellation: cancellation(goos),
		RowUsage:        usageBy,
		RowModelDiscovery: {Status: Unsupported, By: "hrn exec protocol v1",
			Detail: "hello lists no models and no CLI is asked; a model is passed as given, checked only as a safe argument, and the CLI refuses one it does not know"},
		RowTimeLimit: {Status: Enforced, By: "hrn: the task's timeout ends the run and its process tree",
			Test:   "Harness at the pin: internal/agent TestCLIStopsOnTimeout, TestCLICancellationAndTimeoutKillTheGrandchild",
			Detail: "passed only when the start sets timeout"},
		RowTurnLimit: turns,
	}
}

func withModes(shared map[string]Cell, modes map[string]map[string]Cell) []Mode {
	var out []Mode
	for _, name := range []string{"readonly", "auto", "yolo"} {
		cells := map[string]Cell{}
		maps.Copy(cells, shared)
		maps.Copy(cells, modes[name])
		out = append(out, Mode{Mode: name, Offered: true, Cells: cells})
	}
	return append(out, Mode{Mode: "ask", Reason: "mode ask needs approvals, which the pinned engine does not have (#7)"})
}

// Capabilities is the static table for the pinned engine, recorded in the decisions log
// (2026-10-25) with its sources, for the host's OS. Each call returns a fresh copy.
func Capabilities() Matrix { return capabilitiesFor(runtime.GOOS) }

func capabilitiesFor(goos string) Matrix {
	notOffered := func(detail string) Cell { return Cell{Status: Unsupported, Detail: detail} }
	claudeNetwork := Cell{Status: Unsupported, cli: true, Detail: "claude has no network sandbox: its web tools and any command it runs use the user's network"}
	claude := Backend{
		Backend: "claude", CLIVersion: "2.1.284", Observed: recorded,
		Efforts: efforts, Options: []string{"model", "mode", "effort", "timeout", "max_turns", "mcp"}, Models: []string{"claude-opus-5-5"},
		Modes: withModes(common(goos,
			Cell{Status: Requested, By: "claude CLI (its result line: turns, tokens, cost)", cli: true, Test: liveReadonly, Detail: "hrn's result carries what the CLI reports; Ada neither stores nor checks it yet"},
			Cell{Status: Requested, By: "claude --max-turns", cli: true, Test: liveReadonly, Detail: "passed only when the start sets max_turns; the CLI counts the turns"},
		), map[string]map[string]Cell{
			"readonly": {
				RowReadOnly: {Status: Requested, By: "claude --permission-mode plan", cli: true, Test: liveReadonly,
					Detail: "plan mode refuses edits and changing commands, but it is the CLI's own rule, not a sandbox: it writes its plan file under the user's profile (~/.claude/plans), outside the worktree, and project hooks run in readonly mode (live run, 2026-10-25)"},
				RowWorkspaceWrites: notOffered("readonly asks for no writes"),
				RowNetwork:         claudeNetwork,
			},
			"auto": {
				RowReadOnly: notOffered("auto lets the agent edit"),
				RowWorkspaceWrites: {Status: Requested, By: "claude --permission-mode acceptEdits", cli: true,
					Detail: "edits are accepted; a command no allow rule covers is refused, since no one can approve it; nothing keeps writes inside the worktree"},
				RowNetwork: claudeNetwork,
			},
			"yolo": {
				RowReadOnly: notOffered("yolo lets the agent do anything"),
				RowWorkspaceWrites: {Status: Requested, By: "claude --permission-mode bypassPermissions", cli: true,
					Detail: "every tool runs without asking, as the user, on the host; nothing keeps it inside the worktree"},
				RowNetwork: claudeNetwork,
			},
		}),
	}
	codex := Backend{
		Backend: "codex", CLIVersion: "0.163.0-alpha.11", Observed: recorded,
		Efforts: efforts, Options: []string{"model", "mode", "effort", "timeout"}, Models: []string{"gpt-6.1-sol", "gpt-6-luna"},
		Modes: withModes(common(goos,
			Cell{Status: Requested, By: "codex CLI (turn.completed usage: tokens)", cli: true, Test: liveReadonly, Detail: "hrn's result carries the tokens the CLI reports; Ada neither stores nor checks them yet"},
			Cell{Status: Unsupported, Detail: "codex exec has no turn limit, and the pinned hrn does not offer max_turns for codex; use timeout"},
		), map[string]map[string]Cell{
			"readonly": {
				RowReadOnly: {Status: Enforced, By: "codex --sandbox read-only", cli: true, Test: liveDenied,
					Detail: "codex's own sandbox denied the live run's write in the worktree (0.163.0-alpha.11, 2026-10-25)"},
				RowWorkspaceWrites: notOffered("readonly asks for no writes"),
				RowNetwork:         {Status: Requested, By: "codex --sandbox read-only", cli: true, Detail: "the sandbox turns the network off for the commands it runs"},
			},
			"auto": {
				RowReadOnly: notOffered("auto lets the agent edit"),
				RowWorkspaceWrites: {Status: Requested, By: "codex --sandbox workspace-write", cli: true,
					Detail: "writes inside the worktree and temporary directories; the 2026-10-11 runs saw writes outside it refused (Go's cache, the home directory)"},
				RowNetwork: {Status: Requested, By: "codex --sandbox workspace-write", cli: true, Detail: "the sandbox keeps the network off for commands unless the CLI's own configuration allows it"},
			},
			"yolo": {
				RowReadOnly: notOffered("yolo lets the agent do anything"),
				RowWorkspaceWrites: {Status: Requested, By: "codex --dangerously-bypass-approvals-and-sandbox", cli: true,
					Detail: "no sandbox: every command runs as the user, on the host"},
				RowNetwork: {Status: Unsupported, cli: true, Detail: "no sandbox, so nothing restricts the network"},
			},
		}),
	}
	return Matrix{
		Engine: pinnedRevision, Recorded: recorded,
		Checks: Cell{Status: Unsandboxed, By: "adad (M4.2)",
			Detail: "checks run the repository's code and the agent's changes as the user, on the host, outside any backend sandbox and with network (decisions log, 2026-10-20)"},
		Backends: []Backend{claude, codex},
	}
}

func (m Matrix) backend(name string) *Backend {
	for i := range m.Backends {
		if m.Backends[i].Backend == name {
			return &m.Backends[i]
		}
	}
	return nil
}

func (b *Backend) mode(name string) *Mode {
	for i := range b.Modes {
		if b.Modes[i].Mode == name {
			return &b.Modes[i]
		}
	}
	return nil
}

func (b *Backend) cell(mode, row string) Cell {
	if m := b.mode(mode); m != nil {
		return m.Cells[row]
	}
	return Cell{}
}

// Check refuses a start the pinned engine and its backends cannot honour, with the reason.
func (m Matrix) Check(o StartOptions) error {
	b := m.backend(o.Backend)
	if b == nil {
		var names []string
		for _, b := range m.Backends {
			names = append(names, b.Backend)
		}
		return &UnsupportedError{fmt.Sprintf("backend %q is not one Ada runs (%s)", o.Backend, strings.Join(names, ", "))}
	}
	mode := b.mode(o.Mode)
	switch {
	case mode == nil:
		return &UnsupportedError{fmt.Sprintf("the %s backend has no mode %q", o.Backend, o.Mode)}
	case !mode.Offered:
		return &UnsupportedError{mode.Reason}
	case o.Model != "" && !slices.Contains(b.Options, "model"):
		return &UnsupportedError{fmt.Sprintf("the %s backend cannot choose a model", o.Backend)}
	case o.Effort != "" && !slices.Contains(b.Efforts, o.Effort):
		return &UnsupportedError{fmt.Sprintf("the %s backend takes effort %s, not %q", o.Backend, strings.Join(b.Efforts, ", "), o.Effort)}
	case o.MaxTurns > 0 && mode.Cells[RowTurnLimit].Status == Unsupported:
		return &UnsupportedError{fmt.Sprintf("the %s backend cannot limit turns: %s", o.Backend, mode.Cells[RowTurnLimit].Detail)}
	case o.Timeout > 0 && mode.Cells[RowTimeLimit].Status == Unsupported:
		return &UnsupportedError{fmt.Sprintf("the %s backend cannot limit time", o.Backend)}
	}
	return nil
}

// WithInstalled returns a copy of m showing what this host has. Cells that depend on the CLI read
// unverified when its installed version is not the recorded one, when no CLI was found, or when
// the engine's hello advertised other modes or options than the table.
func (m Matrix) WithInstalled(installed map[string]Installed) Matrix {
	out := m
	out.Backends = make([]Backend, len(m.Backends))
	for i, b := range m.Backends {
		in, ok := installed[b.Backend]
		if !ok {
			in = Installed{Reason: "not probed"}
		}
		modes := make([]Mode, len(b.Modes))
		verified := in.Available && in.Version == b.CLIVersion && sameAnswers(b, in)
		for j, mode := range b.Modes {
			cells := map[string]Cell{}
			for row, c := range mode.Cells {
				if c.cli && !verified {
					c.Recorded, c.Status = c.Status, Unverified
				}
				cells[row] = c
			}
			mode.Cells = cells
			if len(cells) == 0 {
				mode.Cells = nil
			}
			modes[j] = mode
		}
		b.Modes = modes
		copied := in
		b.Installed = &copied
		out.Backends[i] = b
	}
	return out
}

// sameAnswers reports whether the engine's hello offered the table's modes and options.
func sameAnswers(b Backend, in Installed) bool {
	var offered []string
	for _, mode := range b.Modes {
		if mode.Offered {
			offered = append(offered, mode.Mode)
		}
	}
	return slices.Equal(sorted(in.Modes), sorted(offered)) && slices.Equal(sorted(in.Options), sorted(b.Options))
}

func sorted(items []string) []string {
	out := slices.Clone(items)
	slices.Sort(out)
	return out
}

var cliVersion = regexp.MustCompile(`[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}(-[0-9A-Za-z.]{1,32})?`)

// ParseCLIVersion takes the version from a CLI's version output: the first dotted triple, with
// any pre-release suffix. Nothing else of the output is kept.
func ParseCLIVersion(output string) string {
	return cliVersion.FindString(output)
}
