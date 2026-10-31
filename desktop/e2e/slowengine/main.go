//go:build fakeengine

// A test-only protocol shim: the existing fake engine does the work, while this
// peer deliberately takes four seconds to forward cancellation or stdin EOF.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
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
	var config struct {
		Engine string
		Marker string
	}
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return err
	}
	// Only runs are slow and marked; the experience card's bot stats must not take the run's marker.
	if len(os.Args) > 1 && os.Args[1] == "bot" {
		child := exec.Command(config.Engine, os.Args[1:]...)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		return child.Run()
	}
	created, err := creationTime()
	if err != nil {
		return err
	}
	identity, err := json.Marshal(struct {
		PID     int
		Created string
	}{os.Getpid(), created})
	if err != nil {
		return err
	}
	marker := config.Marker
	if err = os.WriteFile(marker+".started", identity, 0600); err != nil {
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
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		delayed := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, `"cancel"`) {
				time.Sleep(4 * time.Second)
				delayed = true
			}
			if _, err := fmt.Fprintln(input, line); err != nil {
				break
			}
		}
		if !delayed {
			time.Sleep(4 * time.Second)
		}
		input.Close()
	}()
	if err = child.Wait(); err != nil {
		return err
	}
	// adad must wait for this process, not merely the receipt of a cancel/result.
	return os.WriteFile(marker, identity, 0600)
}
