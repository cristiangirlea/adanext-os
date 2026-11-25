//go:build fakeengine

// Test-only project/membership seeding and lifecycle observation. No production API.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/ipc"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 3 {
		return errors.New("usage: testseed seed|users|replay|holder|job|cert state-dir|dir [name|pid]")
	}
	if os.Args[1] == "cert" {
		return cert(os.Args[2])
	}
	if os.Args[1] == "job" {
		return job(os.Args[2])
	}
	if os.Args[1] == "holder" {
		if len(os.Args) != 4 {
			return errors.New("holder needs endpoint name")
		}
		lock, err := ipc.Acquire(ipc.Config{StateDir: os.Args[2], Name: os.Args[3]})
		var running *ipc.AlreadyRunningError
		if errors.As(err, &running) {
			return json.NewEncoder(os.Stdout).Encode(running)
		}
		if err != nil {
			return err
		}
		defer lock.Release()
		return json.NewEncoder(os.Stdout).Encode(nil)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(os.Args[2], "adanext.db"))
	if err != nil {
		return err
	}
	defer s.Close()
	if os.Args[1] == "seed" {
		_, err = s.Do(ctx, "local", "e2e-seed", "e2e-seed", nil, func(tx *store.Tx) ([]byte, error) {
			at := time.Now().UTC().Format(time.RFC3339Nano)
			for _, q := range []string{
				"INSERT OR IGNORE INTO users(id,name,created) VALUES('local','Local',?)",
				"INSERT INTO projects(id,name,source_path,created) VALUES('project','Test project','never-executed',?)",
				"INSERT INTO project_members(project,user,role,added) VALUES('project','local','owner',?)",
			} {
				if _, err := tx.ExecContext(ctx, q, at); err != nil {
					return nil, err
				}
			}
			return []byte("ok"), nil
		})
		return err
	}
	if os.Args[1] == "users" {
		return users(ctx, s)
	}
	if os.Args[1] != "replay" {
		return errors.New("unknown testseed command")
	}
	if err := s.CreateTask(ctx, "local", "replay", "project", "Long replay", ""); err != nil {
		return err
	}
	if err := s.CreateAttempt(ctx, store.Attempt{ID: "replay-attempt", Task: "replay", Actor: "local", Backend: "codex", Mode: "readonly", Executor: "local", State: "ended_ok"}); err != nil {
		return err
	}
	_, err = s.Do(ctx, "local", "replay-events", "replay-events", nil, func(tx *store.Tx) ([]byte, error) {
		payload, _ := json.Marshal(strings.Repeat("replay ", 2048))
		for n := 1; n <= 900; n++ {
			if err := tx.AppendEvent(ctx, store.Event{Attempt: "replay-attempt", Seq: int64(n), Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "text", Payload: payload}); err != nil {
				return nil, err
			}
		}
		return []byte("ok"), nil
	})
	return err
}

// users adds a second account, sam, beside the owner seed made: each with a project holding a
// task and an empty project, and neither a member of the other's.
func users(ctx context.Context, s *store.Store) error {
	_, err := s.Do(ctx, "local", "e2e-users", "e2e-users", nil, func(tx *store.Tx) ([]byte, error) {
		at := time.Now().UTC().Format(time.RFC3339Nano)
		for _, q := range []string{
			"INSERT INTO users(id,name,created) VALUES('sam','sam',?)",
			"INSERT INTO projects(id,name,source_path,created) VALUES('owner-empty','Owner empty project','never-executed',?)",
			"INSERT INTO project_members(project,user,role,added) VALUES('owner-empty','local','owner',?)",
			"INSERT INTO projects(id,name,source_path,created) VALUES('sam-project','Sam project','never-executed',?)",
			"INSERT INTO project_members(project,user,role,added) VALUES('sam-project','sam','owner',?)",
			"INSERT INTO projects(id,name,source_path,created) VALUES('sam-empty','Sam empty project','never-executed',?)",
			"INSERT INTO project_members(project,user,role,added) VALUES('sam-empty','sam','owner',?)",
		} {
			if _, err := tx.ExecContext(ctx, q, at); err != nil {
				return nil, err
			}
		}
		return []byte("ok"), nil
	})
	if err != nil {
		return err
	}
	if err := s.CreateTask(ctx, "local", "owner-task", "project", "Owner task", ""); err != nil {
		return err
	}
	return s.CreateTask(ctx, "sam", "sam-task", "sam-project", "Sam task", "")
}

// cert writes a test-only pair for localhost into dir: an ECDSA P-256 self-signed leaf valid
// from an hour ago for a day, as cert.pem and key.pem, owner-only. It never overwrites a file.
func cert(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := artifact.PrivateDirectory(dir); err != nil {
		return err
	}
	// Only the files this run created are removed on a failure, so a retry can succeed.
	var created []string
	for _, file := range []struct {
		name string
		data []byte
	}{
		{"cert.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})},
		{"key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})},
	} {
		path := filepath.Join(dir, file.name)
		made, err := writeNew(path, file.data)
		if made {
			created = append(created, path)
		}
		if err != nil {
			for _, p := range created {
				if rmErr := removeFile(p); rmErr != nil {
					err = errors.Join(err, fmt.Errorf("remove %s: %w", p, rmErr))
				}
			}
			return err
		}
	}
	return nil
}

// removeFile is os.Remove; a test makes it fail.
var removeFile = os.Remove

// writeNew creates path owner-only and refuses one that exists; made says whether it created it.
func writeNew(path string, data []byte) (made bool, err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return true, err
	}
	return true, f.Close()
}
