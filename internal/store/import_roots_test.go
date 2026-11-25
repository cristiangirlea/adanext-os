package store

import (
	"slices"
	"testing"
)

func TestImportRootGrantsSurviveReopenAndEndWithTheRoot(t *testing.T) {
	s := openTestStore(t)
	users(t, s, "owner", "member")
	if err := s.write(ctx, "owner", func(tx *Tx) error {
		if err := tx.AddImportRoot(ctx, "r", "/roots", "Roots"); err != nil {
			return err
		}
		return tx.SetImportRootGrant(ctx, "r", "member", true)
	}); err != nil {
		t.Fatal(err)
	}
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, u := range []string{"member", "owner"} {
		roots, err := s.GrantedImportRoots(ctx, u)
		if err != nil || (u == "owner" && len(roots) != 0) || (u == "member" && (len(roots) != 1 || roots[0].ID != "r")) {
			t.Fatalf("%s: %v %v", u, roots, err)
		}
	}
	roots, err := s.AllImportRoots(ctx)
	if err != nil || len(roots) != 1 || !slices.Equal(roots[0].Grants, []string{"member"}) {
		t.Fatal(roots, err)
	}
	if err := s.write(ctx, "owner", func(tx *Tx) error { return tx.RemoveImportRoot(ctx, "r") }); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM import_root_grants").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
