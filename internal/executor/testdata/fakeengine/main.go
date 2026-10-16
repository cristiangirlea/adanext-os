//go:build fakeengine

// A test-only hrn exec v1 peer. It never calls a model or executes a task's text.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 4 || os.Args[1] != "exec" || os.Args[2] != "--protocol" || os.Args[3] != "v1" {
		os.Exit(2)
	}
	seq := 0
	line := func(kind string, fields map[string]any) {
		seq++
		fields["v"], fields["seq"], fields["type"], fields["time"] = 1, seq, kind, time.Now().UTC().Format(time.RFC3339Nano)
		if err := json.NewEncoder(os.Stdout).Encode(fields); err != nil {
			os.Exit(1)
		}
	}
	var backends []any
	for _, name := range []string{"claude", "codex"} {
		backends = append(backends, map[string]any{"name": name, "available": true, "modes": []string{"readonly", "auto"}, "options": []string{"mode", "model", "effort"}, "approvals": false})
	}
	line("hello", map[string]any{"protocol": "v1", "pid": os.Getpid(), "backends": backends})
	decoder := json.NewDecoder(os.Stdin)
	var start struct {
		Type      string `json:"type"`
		AttemptID string `json:"attempt_id"`
		Task      struct {
			Prompt string `json:"prompt"`
		} `json:"task"`
	}
	if err := decoder.Decode(&start); err != nil || start.Type != "start" || start.AttemptID == "" {
		os.Exit(2)
	}
	fields := func() map[string]any {
		return map[string]any{"attempt_id": start.AttemptID, "run_id": "fake-" + start.AttemptID}
	}
	line("accepted", fields())
	cancel := make(chan struct{})
	go func() {
		defer close(cancel)
		for {
			var next struct {
				Type      string `json:"type"`
				AttemptID string `json:"attempt_id"`
			}
			if err := decoder.Decode(&next); err != nil {
				return
			}
			if next.Type == "cancel" && next.AttemptID == start.AttemptID {
				return
			}
		}
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	finish := func(status string) { f := fields(); f["result"] = map[string]any{"status": status}; line("result", f) }
	for n := 1; ; n++ {
		select {
		case <-cancel:
			finish("canceled")
			return
		case <-ticker.C:
			f := fields()
			f["event"] = map[string]any{"kind": "text", "text": fmt.Sprintf("tick %04d", n)}
			line("event", f)
			if strings.HasPrefix(start.Task.Prompt, "complete") && n == 20 {
				finish("ok")
				return
			}
		}
	}
}
