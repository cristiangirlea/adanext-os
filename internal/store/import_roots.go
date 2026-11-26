package store

import (
	"context"
	"database/sql"
	"errors"
)

var ErrRootExists = errors.New("this import root already exists")

type ImportRoot struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type ManagedImportRoot struct {
	ImportRoot
	Path   string   `json:"path"`
	Grants []string `json:"grants"`
}

func (tx *Tx) AddImportRoot(ctx context.Context, id, path, label string) error {
	res, err := tx.tx.ExecContext(ctx, "INSERT INTO import_roots(id,path,label,created) VALUES(?,?,?,?) ON CONFLICT(path) DO NOTHING", id, path, label, now())
	return requireRootChange(res, err, ErrRootExists)
}

func requireRootChange(res sql.Result, err, missing error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return missing
	}
	return err
}

func (tx *Tx) RemoveImportRoot(ctx context.Context, id string) error {
	res, err := tx.tx.ExecContext(ctx, "DELETE FROM import_roots WHERE id=?", id)
	return requireRootChange(res, err, ErrNotFound)
}

func (tx *Tx) SetImportRootGrant(ctx context.Context, id, user string, grant bool) error {
	if !grant {
		res, err := tx.tx.ExecContext(ctx, "DELETE FROM import_root_grants WHERE root=? AND user=?", id, user)
		return requireRootChange(res, err, ErrNotFound)
	}
	var one int
	if err := tx.tx.QueryRowContext(ctx, "SELECT 1 FROM import_roots, users WHERE import_roots.id=? AND users.id=?", id, user).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	_, err := tx.tx.ExecContext(ctx, "INSERT INTO import_root_grants(root,user,granted) VALUES(?,?,?) ON CONFLICT(root,user) DO NOTHING", id, user, now())
	return err
}

func (s *Store) GrantedImportRoots(ctx context.Context, user string) ([]ImportRoot, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT r.id,r.label FROM import_roots r JOIN import_root_grants g ON g.root=r.id WHERE g.user=? ORDER BY r.label,r.id", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ImportRoot{}
	for rows.Next() {
		var r ImportRoot
		if err := rows.Scan(&r.ID, &r.Label); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func grantedRootPath(ctx context.Context, q rowQuerier, user, id string) (string, error) {
	var path string
	err := q.QueryRowContext(ctx, "SELECT r.path FROM import_roots r JOIN import_root_grants g ON g.root=r.id WHERE g.user=? AND r.id=?", user, id).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return path, err
}

func (s *Store) GrantedRootPath(ctx context.Context, user, id string) (string, error) {
	return grantedRootPath(ctx, s.db, user, id)
}

// ImportRootPath is the boundary of an already-authorized project source.
func (s *Store) ImportRootPath(ctx context.Context, id string) (string, error) {
	var path string
	err := s.db.QueryRowContext(ctx, "SELECT path FROM import_roots WHERE id=?", id).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return path, err
}

func (s *Store) AllImportRoots(ctx context.Context) ([]ManagedImportRoot, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT r.id,r.label,r.path,g.user FROM import_roots r LEFT JOIN import_root_grants g ON g.root=r.id ORDER BY r.label,r.id,g.user")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagedImportRoot{}
	for rows.Next() {
		var r ManagedImportRoot
		var user sql.NullString
		if err := rows.Scan(&r.ID, &r.Label, &r.Path, &user); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != r.ID {
			r.Grants = []string{}
			out = append(out, r)
		}
		if user.Valid {
			last := &out[len(out)-1]
			last.Grants = append(last.Grants, user.String)
		}
	}
	return out, rows.Err()
}
