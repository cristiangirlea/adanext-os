//go:build fakeengine

// A test-only protocol shim for the workspace flow: the existing fake engine does the protocol,
// while this peer, on the start message, writes the agent's changes into the task's working
// directory (the attempt's worktree), so Ada has something to commit, check and show. It runs
// no model and executes nothing from the task's text.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// edits are the agent's changes: a patch holding HTML, and a new file whose name holds a
// right-to-left override, which the UI must mark.
var edits = map[string]string{
	"README.md":             "# fixture\n\nEdited by the agent: <b>bold</b> <script>alert(1)</script>\n",
	"notes/rtl\u202etxt.md": "named to look like a .txt file\n",
}

func run() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	configBytes, err := os.ReadFile(self + ".json")
	if err != nil {
		return err
	}
	var config struct{ Engine string }
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return err
	}
	child := exec.Command(config.Engine, os.Args[1:]...)
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	input, err := child.StdinPipe()
	if err != nil {
		return err
	}
	if err = child.Start(); err != nil {
		return err
	}
	go func() {
		defer input.Close()
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			var start struct {
				Type string `json:"type"`
				Task struct {
					Dir string `json:"dir"`
				} `json:"task"`
			}
			if json.Unmarshal(line, &start) == nil && start.Type == "start" && start.Task.Dir != "" {
				for name, content := range edits {
					path := filepath.Join(start.Task.Dir, filepath.FromSlash(name))
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						fmt.Fprintln(os.Stderr, err)
					} else if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
						fmt.Fprintln(os.Stderr, err)
					}
				}
			}
			if _, err := input.Write(append(line, '\n')); err != nil {
				return
			}
		}
	}()
	return child.Wait()
}
