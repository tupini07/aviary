package controlplane

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestValidID(t *testing.T) {
	valid := []string{"a", "alpha", "alpha-1", "a0", "project-with-hyphens", "0abc"}
	invalid := []string{"", "-leading", "UPPER", "has_underscore", "white space", "with.dot", "über"}

	for _, id := range valid {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
	}
	for _, id := range invalid {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
	}
}

func TestCreateAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p, err := s.Create(ctx, "alpha", "Alpha Project")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.ID != "alpha" || p.Name != "Alpha Project" {
		t.Fatalf("unexpected project: %+v", p)
	}
	if p.Status != StatusActive {
		t.Errorf("status = %q, want %q", p.Status, StatusActive)
	}
	if p.CreatedAt.IsZero() || !p.CreatedAt.Equal(p.UpdatedAt) {
		t.Errorf("timestamps not set correctly: created=%v updated=%v", p.CreatedAt, p.UpdatedAt)
	}

	got, err := s.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != p.ID || got.Name != p.Name || got.Status != p.Status {
		t.Errorf("Get mismatch: got %+v want %+v", got, p)
	}
	if !got.CreatedAt.Equal(p.CreatedAt) {
		t.Errorf("CreatedAt round-trip mismatch: got %v want %v", got.CreatedAt, p.CreatedAt)
	}
}

func TestCreateInvalidID(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Create(context.Background(), "Bad ID", ""); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("Create with bad id: got %v, want ErrInvalidID", err)
	}
}

func TestCreateDuplicate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, "dup", ""); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := s.Create(ctx, "dup", ""); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create: got %v, want ErrExists", err)
	}
}

func TestGetNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: got %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if got, err := s.List(ctx); err != nil || len(got) != 0 {
		t.Fatalf("List empty: got %v err %v, want empty slice", got, err)
	}

	for _, id := range []string{"beta", "alpha", "gamma"} {
		if _, err := s.Create(ctx, id, ""); err != nil {
			t.Fatalf("Create %q: %v", id, err)
		}
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"alpha", "beta", "gamma"} // ordered by id
	if len(got) != len(want) {
		t.Fatalf("List len = %d, want %d", len(got), len(want))
	}
	for i, p := range got {
		if p.ID != want[i] {
			t.Errorf("List[%d].ID = %q, want %q", i, p.ID, want[i])
		}
	}
}

func TestSetStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, "alpha", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.SetStatus(ctx, "alpha", StatusDisabled); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, err := s.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusDisabled {
		t.Errorf("status = %q, want %q", got.Status, StatusDisabled)
	}
	if !got.UpdatedAt.After(got.CreatedAt) {
		t.Errorf("UpdatedAt (%v) should be after CreatedAt (%v)", got.UpdatedAt, got.CreatedAt)
	}

	if err := s.SetStatus(ctx, "missing", StatusActive); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetStatus missing: got %v, want ErrNotFound", err)
	}
}

func TestSetSPA(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p, err := s.Create(ctx, "alpha", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.SPA {
		t.Fatalf("new project should default to spa=false")
	}

	if err := s.SetSPA(ctx, "alpha", true); err != nil {
		t.Fatalf("SetSPA: %v", err)
	}
	got, err := s.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.SPA {
		t.Errorf("spa = %v, want true", got.SPA)
	}

	if err := s.SetSPA(ctx, "alpha", false); err != nil {
		t.Fatalf("SetSPA off: %v", err)
	}
	got, _ = s.Get(ctx, "alpha")
	if got.SPA {
		t.Errorf("spa = %v, want false", got.SPA)
	}

	if err := s.SetSPA(ctx, "missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetSPA missing: got %v, want ErrNotFound", err)
	}
}

func TestSetQuota(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p, err := s.Create(ctx, "alpha", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.QuotaBytes != 0 {
		t.Fatalf("new project should default to quota=0 (unlimited), got %d", p.QuotaBytes)
	}

	if err := s.SetQuota(ctx, "alpha", 5<<20); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}
	got, err := s.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.QuotaBytes != 5<<20 {
		t.Errorf("quota = %d, want %d", got.QuotaBytes, 5<<20)
	}

	// Negative quotas clamp to 0 (unlimited).
	if err := s.SetQuota(ctx, "alpha", -1); err != nil {
		t.Fatalf("SetQuota negative: %v", err)
	}
	got, _ = s.Get(ctx, "alpha")
	if got.QuotaBytes != 0 {
		t.Errorf("negative quota = %d, want 0", got.QuotaBytes)
	}

	if err := s.SetQuota(ctx, "missing", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetQuota missing: got %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, "alpha", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete(ctx, "alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: got %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing: got %v, want ErrNotFound", err)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	if _, err := s1.Create(context.Background(), "alpha", "Persisted"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer s2.Close()

	got, err := s2.Get(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Name != "Persisted" {
		t.Errorf("Name = %q, want %q", got.Name, "Persisted")
	}
}
