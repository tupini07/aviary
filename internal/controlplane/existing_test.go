package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenExistingPreservesSchemaAndSessionKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state ?#email@example.test.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.SessionKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("CREATE TABLE legacy(value BLOB); INSERT INTO legacy VALUES(x'00ff')"); err != nil {
		t.Fatal(err)
	}
	store.Close()
	managed, err := OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	defer managed.Close()
	actual, err := managed.SessionKey(context.Background())
	if err != nil || string(actual) != string(key) {
		t.Fatal("session key changed:", err)
	}
	var value []byte
	if err := managed.db.QueryRow("SELECT value FROM legacy").Scan(&value); err != nil || string(value) != "\x00\xff" {
		t.Fatal("legacy table changed:", err)
	}
}

func TestOpenExistingRefusesMissingSchemaAndSessionAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	if _, err := OpenExisting(path); err == nil {
		t.Fatal("created missing store")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing store was created")
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, err := OpenExisting(path); err == nil {
		t.Fatal("generated a new session authority")
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionKey(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("DROP TABLE api_keys"); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, err := OpenExisting(path); err == nil {
		t.Fatal("migrated missing schema")
	}
	store, err = open(path, true)
	if err == nil {
		store.Close()
		t.Fatal("invalid existing schema accepted")
	}
}
