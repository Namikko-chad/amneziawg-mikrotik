package profiles

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateLegacy(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "awg0.conf")
	os.WriteFile(legacy, []byte("[Interface]\n"), 0o600)

	s, err := Open(filepath.Join(dir, "profiles.json"), legacy)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Active()
	if err != nil || p.Config != "[Interface]\n" || p.Source != "migrated" {
		t.Fatalf("active = %+v, %v", p, err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy config not removed: %v", err)
	}

	// Reopen: state persists, legacy is not re-imported.
	s2, err := Open(filepath.Join(dir, "profiles.json"), legacy)
	if err != nil {
		t.Fatal(err)
	}
	if l, active := s2.List(); len(l) != 1 || active != p.ID {
		t.Fatalf("reopen: %d profiles, active %q", len(l), active)
	}
}

func TestLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Active(); err != ErrNotFound {
		t.Fatalf("empty store active: %v", err)
	}

	a, _ := s.Add("A", "text", "a", false)
	if _, active := s.List(); active != a.ID {
		t.Fatal("first profile must become active")
	}
	b, _ := s.Add("", "text", "b", false)
	if b.Name == "" {
		t.Fatal("default name not set")
	}
	if _, active := s.List(); active != a.ID {
		t.Fatal("activate=false must keep the active profile")
	}
	if _, err := s.SetActive(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(b.ID, " B "); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Get(b.ID); p.Name != "B" {
		t.Fatalf("rename: %q", p.Name)
	}

	was, err := s.Delete(b.ID)
	if err != nil || !was {
		t.Fatalf("delete active: %v %v", was, err)
	}
	if _, active := s.List(); active != a.ID {
		t.Fatal("active must fall back to remaining profile")
	}
	if was, _ := s.Delete(a.ID); !was {
		t.Fatal("expected wasActive")
	}
	if _, err := s.Delete(a.ID); err != ErrNotFound {
		t.Fatalf("double delete: %v", err)
	}

	s2, _ := Open(path, "")
	if l, active := s2.List(); len(l) != 0 || active != "" {
		t.Fatalf("after reopen: %d, %q", len(l), active)
	}
}

func TestBackup(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "profiles.json"), "")
	a, _ := s.Add("A", "text", "a", true)
	b, _ := s.Add("B", "text", "b", false)
	c, _ := s.Add("C", "text", "c", false)

	if err := s.SetBackup(a.ID); err == nil {
		t.Fatal("active profile accepted as backup")
	}
	if err := s.SetBackup(b.ID); err != nil {
		t.Fatal(err)
	}
	// Selecting the backup swaps roles.
	s.SetActive(b.ID)
	if bk, _ := s.Backup(); bk.ID != a.ID {
		t.Fatalf("backup after swap = %q", bk.Name)
	}
	// Deleting the active profile promotes the backup.
	if was, _ := s.Delete(b.ID); !was {
		t.Fatal("expected wasActive")
	}
	if act, _ := s.Active(); act.ID != a.ID {
		t.Fatalf("active after delete = %q", act.Name)
	}
	if _, err := s.Backup(); err != ErrNotFound {
		t.Fatal("backup must be cleared after promotion")
	}
	// Deleting the backup clears it.
	s.SetBackup(c.ID)
	s.Delete(c.ID)
	if _, err := s.Backup(); err != ErrNotFound {
		t.Fatal("deleted backup still set")
	}
	if err := s.SetBackup(""); err != nil {
		t.Fatal(err)
	}
}
