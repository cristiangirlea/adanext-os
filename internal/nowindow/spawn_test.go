package nowindow

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// The coordinator's code: internal/ and cmd/adad. cmd/ada-live is run from the
// owner's terminal, so its children share that console.
var spawnRoots = []string{"..", "../../cmd/adad"}

// hidden reports whether fn starts the command held in name without a console
// window: through Set, through the executor's process supervisor (which calls
// Set), or with a SysProcAttr naming CREATE_NO_WINDOW or DETACHED_PROCESS.
func hidden(fn ast.Node, name string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if len(n.Args) == 1 && isIdent(n.Args[0], name) && (isCall(n.Fun, "nowindow", "Set") || isIdent(n.Fun, "newProcessSupervisor")) {
				found = true
			}
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "SysProcAttr" || !isIdent(sel.X, name) || i >= len(n.Rhs) {
					continue
				}
				ast.Inspect(n.Rhs[i], func(m ast.Node) bool {
					if s, ok := m.(*ast.SelectorExpr); ok && (s.Sel.Name == "CREATE_NO_WINDOW" || s.Sel.Name == "DETACHED_PROCESS") {
						found = true
					}
					return true
				})
			}
		}
		return !found
	})
	return found
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func isCall(e ast.Expr, pkg, fn string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == fn && isIdent(sel.X, pkg)
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
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					assign, ok := n.(*ast.AssignStmt)
					if !ok {
						return true
					}
					for i, rhs := range assign.Rhs {
						call, ok := rhs.(*ast.CallExpr)
						if !ok || !(isCall(call.Fun, "exec", "Command") || isCall(call.Fun, "exec", "CommandContext")) {
							continue
						}
						sites++
						id, ok := assign.Lhs[i].(*ast.Ident)
						if !ok || !hidden(fn.Body, id.Name) {
							t.Errorf("%s: a command started without nowindow.Set", fset.Position(call.Pos()))
						}
					}
					return true
				})
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
