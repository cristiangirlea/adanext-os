package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExperienceHelper(t *testing.T) {
	mode := os.Getenv("ADA_EXPERIENCE_TEST")
	if mode == "" {
		return
	}
	switch mode {
	case "on":
		fmt.Printf(`{"version":1,"enabled":true,"level":4,"xp":23,"week_start":"2026-10-26","week":{"delivery":2,"practice":3},"streak":null,"days":[%s],"badges":["first"],"exercise":{"kind":"quiz","source":"x","material":"masked","answered":false},"familiarity":null}`, strings.Repeat("false,", 34)+"true")
	case "incomplete":
		fmt.Printf(`{"version":1,"enabled":true,"level":4,"xp":23,"week_start":"2026-10-26","week":{},"streak":null,"days":[%s],"badges":[],"exercise":null,"familiarity":null}`, strings.Repeat("false,", 34)+"true")
	case "extra":
		fmt.Print(`{"version":1,"enabled":false,"future":true}`)
	case "secret":
		if _, ok := os.LookupEnv("FOO_API_KEY"); ok {
			os.Exit(3)
		}
		fmt.Print(`{"version":1,"enabled":false}`)
	case "off":
		fmt.Print(`{"version":1,"enabled":false}`)
	case "version":
		fmt.Print(`{"version":2,"enabled":false}`)
	case "invalid":
		fmt.Print(`{`)
	case "large":
		fmt.Printf(`{"version":1,"enabled":true,"level":1,"xp":1,"week_start":"2026-10-26","week":{"delivery":0,"practice":0},"streak":null,"days":[%s],"badges":["%s"],"exercise":null,"familiarity":null}`, strings.Repeat("false,", 34)+"false", strings.Repeat("x", 256*1024))
	case "hang":
		time.Sleep(time.Minute)
	case "exit":
		fmt.Print(`{"version":1,"enabled":false}`)
		os.Exit(2)
	case "home":
		_ = os.WriteFile(os.Getenv("ADA_EXPERIENCE_HOME_FILE"), []byte(os.Getenv("HRN_HOME")), 0600)
		fmt.Print(`{"version":1,"enabled":false}`)
	}
	os.Exit(0)
}

func TestExperienceRunner(t *testing.T) {
	for _, mode := range []string{"on", "off", "extra", "secret", "version", "invalid", "incomplete", "large", "hang", "exit"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("FOO_API_KEY", "test-secret")
			e, _ := probeExecutor(t, "answer")
			e.experienceArgs = []string{"-test.run=^TestExperienceHelper$"}
			e.experienceTimeout = 200 * time.Millisecond
			e.cfg.Env = append(e.cfg.Env, "ADA_EXPERIENCE_TEST="+mode)
			start := time.Now()
			got, err := e.Experience(context.Background())
			if mode == "hang" && time.Since(start) > 3*time.Second {
				t.Fatal("timeout was not enforced")
			}
			if mode == "on" || mode == "off" || mode == "extra" || mode == "secret" {
				if err != nil || got.Version != 1 || got.Enabled != (mode == "on") {
					t.Fatalf("got %+v, %v", got, err)
				}
				if mode == "on" && (got.Level != 4 || len(got.Days) != 35 || got.Exercise.Material == nil) {
					t.Fatalf("shape: %+v", got)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s succeeded", mode)
			}
		})
	}
}

func TestExperienceHome(t *testing.T) {
	for _, custom := range []bool{true, false} {
		t.Run(fmt.Sprint(custom), func(t *testing.T) {
			home := t.TempDir()
			if custom {
				t.Setenv("HRN_HOME", home)
			} else {
				t.Setenv("HRN_HOME", "")
				user, err := os.UserHomeDir()
				if err != nil {
					t.Fatal(err)
				}
				home = filepath.Join(user, ".hrn")
			}
			path := filepath.Join(t.TempDir(), "home")
			e, _ := probeExecutor(t, "answer")
			e.experienceArgs = []string{"-test.run=^TestExperienceHelper$"}
			e.cfg.Env = append(e.cfg.Env, "ADA_EXPERIENCE_TEST=home", "ADA_EXPERIENCE_HOME_FILE="+path)
			if _, err := e.Experience(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != home {
				t.Fatalf("home %q, %v; want %q", got, err, home)
			}
		})
	}
}
