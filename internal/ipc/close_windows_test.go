//go:build windows

package ipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

func TestClosingThePipeListenerWhileClientsConnectNeverHangs(t *testing.T) {
	for iteration := 0; iteration < 200; iteration++ {
		t.Run(fmt.Sprintf("iteration_%d", iteration), func(t *testing.T) {
			cfg := Config{StateDir: t.TempDir(), Name: fmt.Sprintf("close-%d-%d", os.Getpid(), iteration)}
			lock, err := Acquire(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := lock.Release(); err != nil {
					t.Error(err)
				}
			})
			listener, err := lock.Listen()
			if err != nil {
				t.Fatal(err)
			}
			acceptDone := make(chan struct{})
			t.Cleanup(func() {
				closed := make(chan struct{})
				go func() { _ = listener.Close(); close(closed) }()
				select {
				case <-closed:
				case <-time.After(2 * time.Second):
					t.Error("listener cleanup did not finish")
				}
				select {
				case <-acceptDone:
				case <-time.After(2 * time.Second):
					t.Error("accept goroutine did not stop")
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			stop := make(chan struct{})
			var stopOnce sync.Once
			stopDialers := func() { cancel(); stopOnce.Do(func() { close(stop) }) }

			accepted := make(chan error, 1)
			go func() {
				defer close(acceptDone)
				for {
					conn, err := listener.Accept()
					if err != nil {
						accepted <- err
						return
					}
					_ = conn.Close()
				}
			}()
			attempted := make(chan struct{}, 4)
			finished := make(chan struct{}, 4)
			var dialers sync.WaitGroup
			for dialer := 0; dialer < 4; dialer++ {
				dialers.Add(1)
				go func() {
					defer dialers.Done()
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
			t.Cleanup(func() {
				stopDialers()
				done := make(chan struct{})
				go func() { dialers.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("dialers did not stop during cleanup")
				}
			})
			for dialer := 0; dialer < 4; dialer++ {
				select {
				case <-attempted:
				case <-time.After(2 * time.Second):
					t.Fatal("dialers did not start within 2 seconds")
				}
			}
			closed := make(chan error, 1)
			go func() { closed <- listener.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("close listener: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("listener Close hung for more than 2 seconds")
			}
			stopDialers()
			for dialer := 0; dialer < 4; dialer++ {
				select {
				case <-finished:
				case <-time.After(2 * time.Second):
					t.Fatal("dialers did not stop")
				}
			}
			select {
			case err := <-accepted:
				if !errors.Is(err, net.ErrClosed) {
					t.Fatalf("Accept after Close: %v, want net.ErrClosed", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Accept did not stop after Close")
			}
		})
	}
}
