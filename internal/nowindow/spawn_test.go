package nowindow

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The coordinator's code: internal/ and cmd/adad. cmd/ada-live is run from the
// owner's terminal, so its children share that console.
var spawnRoots = []string{"..", "../../cmd/adad"}

// importName is the name a file uses for the package at path, or "".
func importName(file *ast.File, path string) string {
	for _, spec := range file.Imports {
		if p, _ := strconv.Unquote(spec.Path.Value); p == path {
			if spec.Name != nil {
				return spec.Name.Name
			}
			return path[strings.LastIndex(path, "/")+1:]
		}
	}
	return ""
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func isSel(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && pkg != "" && sel.Sel.Name == name && isIdent(sel.X, pkg)
}

// names reports whether e mentions any of the selector names.
func names(e ast.Node, want ...string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok {
			for _, w := range want {
				if s.Sel.Name == w {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// rootedAt reports whether e is name.SysProcAttr or a field below it.
func rootedAt(e ast.Expr, name string) bool {
	for {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if sel.Sel.Name == "SysProcAttr" && isIdent(sel.X, name) {
			return true
		}
		e = sel.X
	}
}

// hidden reports whether, after the command in name is made at from, the last
// word on its creation flags in body leaves CREATE_NO_WINDOW set: a call to
// nowindow.Set or the executor's process supervisor (which calls Set), or a
// SysProcAttr assignment naming the flag. Any other assignment to SysProcAttr
// or its flags undoes it; `|=` keeps it. The walk ends where name is given
// another command. allowDetached accepts DETACHED_PROCESS, which only adad's
// own relaunch may use.
func hidden(body ast.Node, name string, from token.Pos, allowDetached bool) bool {
	flags := []string{"CREATE_NO_WINDOW"}
	if allowDetached {
		flags = append(flags, "DETACHED_PROCESS")
	}
	state, done := false, false
	ast.Inspect(body, func(n ast.Node) bool {
		if done {
			return false
		}
		if n == nil || n.Pos() < from {
			return true
		}
		switch n := n.(type) {
		case *ast.CallExpr:
			if len(n.Args) == 1 && isIdent(n.Args[0], name) && (isSel(n.Fun, "nowindow", "Set") || isIdent(n.Fun, "newProcessSupervisor")) {
				state = true
			}
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if isIdent(lhs, name) {
					done = true
					return false
				}
				if !rootedAt(lhs, name) {
					continue
				}
				switch n.Tok {
				case token.OR_ASSIGN:
					state = state || (i < len(n.Rhs) && names(n.Rhs[i], flags...))
				case token.ASSIGN:
					state = i < len(n.Rhs) && names(n.Rhs[i], flags...)
				default:
					// &^=, ^= and the rest can clear the flag.
					state = false
				}
			}
		}
		return true
	})
	return state
}

// unhidden returns the spawn sites in file that start a command without
// leaving CREATE_NO_WINDOW set, and how many spawn sites it saw.
func unhidden(fset *token.FileSet, path string, file *ast.File) (bad []string, sites int) {
	execName, osName := importName(file, "os/exec"), importName(file, "os")
	report := func(n ast.Node, why string) {
		bad = append(bad, fset.Position(n.Pos()).String()+": "+why)
	}
	// check finds the spawn calls in one top-level declaration; body is where
	// a command assigned to a name is looked for.
	check := func(decl ast.Node, body ast.Node, allowDetached bool) {
		assigned := map[*ast.CallExpr]*ast.Ident{}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				if len(n.Lhs) == len(n.Rhs) {
					for i, rhs := range n.Rhs {
						if call, ok := rhs.(*ast.CallExpr); ok {
							if id, ok := n.Lhs[i].(*ast.Ident); ok {
								assigned[call] = id
							}
						}
					}
				}
			case *ast.ValueSpec:
				if len(n.Names) == len(n.Values) {
					for i, v := range n.Values {
						if call, ok := v.(*ast.CallExpr); ok {
							assigned[call] = n.Names[i]
						}
					}
				}
			case *ast.CompositeLit:
				if isSel(n.Type, execName, "Cmd") {
					sites++
					report(n, "an exec.Cmd literal; use exec.Command and nowindow.Set")
				}
			case *ast.CallExpr:
				if isSel(n.Fun, osName, "StartProcess") {
					sites++
					report(n, "os.StartProcess; use exec.Command and nowindow.Set")
				}
				if !isSel(n.Fun, execName, "Command") && !isSel(n.Fun, execName, "CommandContext") {
					return true
				}
				sites++
				id := assigned[n]
				if id == nil || id.Name == "_" {
					report(n, "a command not held in a variable that nowindow.Set marks")
				} else if !hidden(body, id.Name, n.End(), allowDetached) {
					report(n, "a command started without nowindow.Set")
				}
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			allowDetached := fn.Name.Name == "detachWith" && filepath.Base(path) == "detach_windows.go"
			check(fn, fn, allowDetached)
			continue
		}
		// Package-level values, func literals among them.
		check(decl, decl, false)
	}
	return bad, sites
}

func TestEverySpawnSiteOfTheCoordinatorStartsWithoutAConsoleWindow(t *testing.T) {
	windowsBuild := build.Default
	windowsBuild.GOOS = "windows"
	sites := 0
	for _, root := range spawnRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// Only code built for Windows can open a console window.
			if ok, err := windowsBuild.MatchFile(filepath.Dir(path), d.Name()); err != nil || !ok {
				return err
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			bad, n := unhidden(fset, path, file)
			sites += n
			for _, b := range bad {
				t.Error(b)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The scan must see the coordinator's spawn sites, or it proves nothing.
	if sites < 8 {
		t.Fatalf("found %d spawn sites, want at least 8", sites)
	}
}

func TestTheSpawnGuardFindsEachFormOfAnUnhiddenCommand(t *testing.T) {
	const head = "package p\n\nimport (\n\t\"os\"\n\t\"os/exec\"\n\t\"syscall\"\n\n\t\"golang.org/x/sys/windows\"\n\t\"github.com/cristiangirlea/ada-os/internal/nowindow\"\n)\n\nvar _ = os.Args\nvar _ syscall.SysProcAttr\nvar _ = windows.CREATE_NO_WINDOW\nvar _ = nowindow.Set\n\n"
	for _, c := range []struct {
		name, file, src string
		bad             int
	}{
		{"set", "a.go", "func f() { cmd := exec.Command(\"git\"); nowindow.Set(cmd); _ = cmd.Run() }", 0},
		{"no set", "a.go", "func f() { cmd := exec.Command(\"git\"); _ = cmd.Run() }", 1},
		{"supervisor", "a.go", "func f() { cmd := exec.Command(\"git\"); newProcessSupervisor(cmd) }", 0},
		{"flag literal", "a.go", "func f() { cmd := exec.Command(\"git\"); cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW} }", 0},
		{"overwritten after set", "a.go", "func f() { cmd := exec.Command(\"git\"); nowindow.Set(cmd); cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED} }", 1},
		{"flags overwritten after set", "a.go", "func f() { cmd := exec.Command(\"git\"); nowindow.Set(cmd); cmd.SysProcAttr.CreationFlags = windows.CREATE_SUSPENDED }", 1},
		{"flags or-ed after set", "a.go", "func f() { cmd := exec.Command(\"git\"); nowindow.Set(cmd); cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED }", 0},
		{"flag cleared after set", "a.go", "func f() { cmd := exec.Command(\"git\"); nowindow.Set(cmd); cmd.SysProcAttr.CreationFlags &^= windows.CREATE_NO_WINDOW }", 1},
		{"name reused for a hidden command", "a.go", "func f() { cmd := exec.Command(\"git\", \"a\"); _ = cmd.Run(); cmd = exec.Command(\"git\", \"b\"); nowindow.Set(cmd); _ = cmd.Run() }", 1},
		{"name reused, both hidden", "a.go", "func f() { cmd := exec.Command(\"git\", \"a\"); nowindow.Set(cmd); cmd = exec.Command(\"git\", \"b\"); nowindow.Set(cmd) }", 0},
		{"set before an earlier make", "a.go", "func f() { var cmd *exec.Cmd; nowindow.Set(cmd); cmd = exec.Command(\"git\") }", 1},
		{"var", "a.go", "func f() { var cmd = exec.Command(\"git\"); _ = cmd.Run() }", 1},
		{"var with set", "a.go", "func f() { var cmd = exec.Command(\"git\"); nowindow.Set(cmd) }", 0},
		{"returned", "a.go", "func f() *exec.Cmd { return exec.Command(\"git\") }", 1},
		{"chained", "a.go", "func f() { _ = exec.Command(\"git\").Run() }", 1},
		{"literal", "a.go", "func f() { _ = &exec.Cmd{Path: \"git\"} }", 1},
		{"start process", "a.go", "func f() { _, _ = os.StartProcess(\"git\", nil, nil) }", 1},
		{"package func literal", "a.go", "var g = func() { cmd := exec.Command(\"git\"); _ = cmd.Run() }", 1},
		{"detached elsewhere", "a.go", "func f() { cmd := exec.Command(\"git\"); cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS} }", 1},
		{"detached relaunch", "detach_windows.go", "func detachWith() { cmd := exec.Command(\"adad\"); cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS} }", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, c.file, head+c.src+"\n", 0)
			if err != nil {
				t.Fatal(err)
			}
			if bad, _ := unhidden(fset, c.file, file); len(bad) != c.bad {
				t.Fatalf("found %d unhidden sites, want %d: %v", len(bad), c.bad, bad)
			}
		})
	}
	// Another name for os/exec is still os/exec.
	src := "package p\n\nimport run \"os/exec\"\n\nfunc f() { cmd := run.Command(\"git\"); _ = cmd.Run() }\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "a.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bad, _ := unhidden(fset, "a.go", file); len(bad) != 1 {
		t.Fatalf("aliased os/exec: found %v, want one unhidden site", bad)
	}
}
