package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAPIKeyStoreCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "alpha", "Alpha"); err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := s.CreateAPIKey(ctx, "k1", "alpha", "ci", "hash-one", nil); err != nil {
		t.Fatalf("create key: %v", err)
	}
	exp := time.Now().Add(24 * time.Hour).UTC()
	if err := s.CreateAPIKey(ctx, "k2", "alpha", "agent", "hash-two", &exp); err != nil {
		t.Fatalf("create key with expiry: %v", err)
	}

	keys, err := s.ListAPIKeys(ctx, "alpha")
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("len(keys) = %d, want 2", len(keys))
	}
	// Newest first: k2 then k1.
	if keys[0].ID != "k2" || keys[1].ID != "k1" {
		t.Fatalf("order = %s, %s; want k2, k1", keys[0].ID, keys[1].ID)
	}
	if keys[0].ExpiresAt == nil {
		t.Fatal("expected k2 to carry an expiry")
	}
	if keys[1].ExpiresAt != nil {
		t.Fatal("expected k1 to have no expiry")
	}
	if keys[0].KeyHash != "" || keys[1].KeyHash != "" {
		t.Fatal("list must not expose key hashes")
	}

	// Lookup by hash.
	k, err := s.APIKeyByHash(ctx, "hash-one")
	if err != nil {
		t.Fatalf("by hash: %v", err)
	}
	if k.ID != "k1" || k.ProjectID != "alpha" {
		t.Fatalf("by hash = %+v", k)
	}
	if _, err := s.APIKeyByHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hash err = %v, want ErrNotFound", err)
	}

	// Touch updates last-used.
	if err := s.TouchAPIKey(ctx, "k1"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	k, _ = s.APIKeyByHash(ctx, "hash-one")
	if k.LastUsedAt == nil {
		t.Fatal("touch did not set last_used_at")
	}

	// Delete is project-scoped.
	if err := s.DeleteAPIKey(ctx, "beta", "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete wrong project err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteAPIKey(ctx, "alpha", "k1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	keys, _ = s.ListAPIKeys(ctx, "alpha")
	if len(keys) != 1 || keys[0].ID != "k2" {
		t.Fatalf("after delete keys = %+v", keys)
	}
}

func TestAPIKeyStoreUniqueHash(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "alpha", ""); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := s.CreateAPIKey(ctx, "k1", "alpha", "", "dup", nil); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.CreateAPIKey(ctx, "k2", "alpha", "", "dup", nil); !errors.Is(err, ErrExists) {
		t.Fatalf("dup hash err = %v, want ErrExists", err)
	}
}

func TestAPIKeyCascadeOnProjectDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "alpha", ""); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := s.CreateAPIKey(ctx, "k1", "alpha", "", "hash", nil); err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := s.Delete(ctx, "alpha"); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if _, err := s.APIKeyByHash(ctx, "hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key survived project delete: %v", err)
	}
}
