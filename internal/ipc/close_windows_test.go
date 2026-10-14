//go:build windows

package ipc

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestClosingThePipeListenerWhileClientsConnectNeverHangs(t *testing.T) {
	for iteration := 0; iteration < 200; iteration++ {
		cfg := Config{StateDir: t.TempDir(), Name: fmt.Sprintf("close-%d-%d", os.Getpid(), iteration)}
		lock, err := Acquire(cfg)
		if err != nil {
			t.Fatalf("iteration %d: acquire lock: %v", iteration, err)
		}
		listener, err := lock.Listen()
		if err != nil {
			_ = lock.Release()
			t.Fatalf("iteration %d: listen: %v", iteration, err)
		}

		accepted := make(chan struct{})
		go func() {
			defer close(accepted)
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()

		ctx, cancel := context.WithCancel(context.Background())
		stop := make(chan struct{})
		attempted := make(chan struct{}, 4)
		finished := make(chan struct{}, 4)
		for dialer := 0; dialer < 4; dialer++ {
			go func() {
				defer func() { finished <- struct{}{} }()
				first := true
				for {
					select {
					case <-stop:
						return
					default:
					}
					conn, err := Dial(ctx, cfg)
					if err == nil {
						_ = conn.Close()
					}
					if first {
						attempted <- struct{}{}
						first = false
					}
				}
			}()
		}

		for dialer := 0; dialer < 4; dialer++ {
			select {
			case <-attempted:
			case <-time.After(2 * time.Second):
				close(stop)
				cancel()
				t.Fatalf("iteration %d: dialers did not start within 2 seconds", iteration)
			}
		}

		closed := make(chan error, 1)
		go func() { closed <- listener.Close() }()
		select {
		case err := <-closed:
			if err != nil {
				close(stop)
				cancel()
				_ = lock.Release()
				t.Fatalf("iteration %d: close listener: %v", iteration, err)
			}
		case <-time.After(2 * time.Second):
			close(stop)
			cancel()
			_ = lock.Release()
			t.Fatalf("iteration %d: listener Close hung for more than 2 seconds", iteration)
		}

		close(stop)
		cancel()
		for dialer := 0; dialer < 4; dialer++ {
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatalf("iteration %d: dialers did not stop", iteration)
			}
		}
		select {
		case <-accepted:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: Accept did not stop after Close", iteration)
		}
		if err := lock.Release(); err != nil {
			t.Fatalf("iteration %d: release lock: %v", iteration, err)
		}
	}
}
