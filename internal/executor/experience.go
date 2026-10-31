package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cristiangirlea/ada-os/internal/api"
	"github.com/cristiangirlea/ada-os/internal/engine"
)

type experienceBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *experienceBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 256*1024 - b.Len()
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func (e *Executor) Experience(ctx context.Context) (api.Experience, error) {
	var result api.Experience
	path, err := filepath.Abs(e.cfg.EnginePath)
	if err != nil {
		return result, err
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(path), ".exe") {
		return result, errors.New("invalid engine path")
	}
	args := e.experienceArgs
	if args == nil {
		args = []string{"bot", "stats", "--json"}
	}
	timeout := e.experienceTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	home := os.Getenv("HRN_HOME")
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return result, err
		}
		home = filepath.Join(home, ".hrn")
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Path = path
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), e.cfg.Env...)
	cmd.Env = append(cmd.Env, "HRN_HOME="+home)
	var out experienceBuffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err = engine.Verify(path, e.cfg.EngineSHA256); err != nil {
		return result, err
	}
	if err = cmd.Run(); err != nil {
		return result, err
	}
	if out.overflow {
		return result, errors.New("experience output too large")
	}
	raw := out.Bytes()
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return result, err
	}
	for _, key := range []string{"version", "enabled"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return result, errors.New("missing experience field")
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&result); err != nil {
		return result, err
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		return result, errors.New("trailing experience output")
	}
	if result.Version != 1 {
		return result, errors.New("unsupported experience version")
	}
	if result.Enabled {
		for _, key := range []string{"level", "xp", "week_start", "week", "streak", "days", "badges", "exercise", "familiarity"} {
			if len(fields[key]) == 0 {
				return result, errors.New("missing experience field")
			}
		}
		for _, key := range []string{"level", "xp", "week_start", "week", "days", "badges"} {
			if string(fields[key]) == "null" {
				return result, errors.New("null experience field")
			}
		}
		if err := validateExperienceParts(fields); err != nil {
			return result, err
		}
		if len(result.Days) != 35 || result.Week == nil {
			return result, errors.New("invalid experience shape")
		}
		if _, err := time.Parse("2006-01-02", result.WeekStart); err != nil {
			return result, err
		}
		if result.Exercise != nil && (result.Exercise.Answered == (result.Exercise.Material != nil)) {
			return result, errors.New("invalid exercise material")
		}
	} else if len(fields) != 2 {
		return result, errors.New("invalid disabled experience")
	}
	return result, nil
}

// Required nested fields must not silently become Go zero values.
func validateExperienceParts(fields map[string]json.RawMessage) error {
	required := func(raw json.RawMessage, keys ...string) (map[string]json.RawMessage, error) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil || m == nil {
			return nil, errors.New("invalid experience object")
		}
		for _, key := range keys {
			if len(m[key]) == 0 || string(m[key]) == "null" {
				return nil, errors.New("missing experience field")
			}
		}
		return m, nil
	}
	if _, err := required(fields["week"], "delivery", "practice"); err != nil {
		return err
	}
	if string(fields["exercise"]) != "null" {
		m, err := required(fields["exercise"], "kind", "source", "answered")
		if err != nil {
			return err
		}
		if v, ok := m["material"]; ok && string(v) == "null" {
			return errors.New("null exercise material")
		}
	}
	if string(fields["familiarity"]) != "null" {
		m, err := required(fields["familiarity"], "known", "unseen")
		if err != nil {
			return err
		}
		for _, key := range []string{"known", "unseen"} {
			var entries []json.RawMessage
			if err := json.Unmarshal(m[key], &entries); err != nil || entries == nil {
				return errors.New("invalid familiarity")
			}
			for _, entry := range entries {
				if _, err := required(entry, "repo", "area", "count"); err != nil {
					return err
				}
			}
		}
	}
	for _, key := range []string{"days", "badges"} {
		var entries []json.RawMessage
		if err := json.Unmarshal(fields[key], &entries); err != nil {
			return err
		}
		for _, entry := range entries {
			if string(entry) == "null" {
				return errors.New("null experience item")
			}
		}
	}
	return nil
}
