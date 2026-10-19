//go:build !windows && !linux

package ipc

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestListenAndDialExplainUnsupportedPeerChecks(t *testing.T) {
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "unsupported"}
	want := "ipc: peer credential checks are not supported on " + runtime.GOOS
	if listener, err := listen(cfg); err == nil || !strings.Contains(err.Error(), want) {
		if listener != nil {
			listener.Close()
		}
		t.Fatalf("Listen error = %v, want %q", err, want)
	}
	if conn, err := Dial(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), want) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("Dial error = %v, want %q", err, want)
	}
}

func TestListenAndDialNameAnOverlongSocketPathOnThisUnixSystem(t *testing.T) {
	cfg := Config{StateDir: filepath.Join(ownerOnlyDir(t), strings.Repeat("a", 104)), Name: "long"}
	path := filepath.Join(cfg.StateDir, "run", cfg.Name+".sock")
	if listener, err := listen(cfg); err == nil || !strings.Contains(err.Error(), path) {
		if listener != nil {
			listener.Close()
		}
		t.Fatalf("Listen error = %v, want path %q", err, path)
	}
	if conn, err := Dial(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), path) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("Dial error = %v, want path %q", err, path)
	}
}
