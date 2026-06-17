package aviary

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// enrollSuperuserPasskey adds a dummy superuser passkey directly via the store
// so tests can exercise the passkey-only toggle without a full WebAuthn ceremony.
func enrollSuperuserPasskey(t *testing.T, av *Aviary, credID string) {
	t.Helper()
	if err := av.store.AddSuperuserPasskey(context.Background(), credID, "test", []byte(`{}`)); err != nil {
		t.Fatalf("AddSuperuserPasskey: %v", err)
	}
}

func TestPasskeyOnlyToggleRequiresPasskey(t *testing.T) {
	av := newTestAviary(t)
	cookie := loginAs(t, av, "admin@example.com", "supersecret")

	// Enabling passkey-only mode without an enrolled passkey is refused.
	rec := doControl(t, av, http.MethodPut, "/api/auth/security",
		securityRequest{PasswordLoginDisabled: true}, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enable without passkey: status %d body %s", rec.Code, rec.Body.String())
	}
	if got, _ := av.store.PasswordLoginDisabled(context.Background()); got {
		t.Fatal("flag should remain false after rejected enable")
	}

	// With a passkey enrolled, enabling succeeds.
	enrollSuperuserPasskey(t, av, "cred-1")
	rec = doControl(t, av, http.MethodPut, "/api/auth/security",
		securityRequest{PasswordLoginDisabled: true}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable with passkey: status %d body %s", rec.Code, rec.Body.String())
	}
	if got, _ := av.store.PasswordLoginDisabled(context.Background()); !got {
		t.Fatal("flag should be true after successful enable")
	}
}

func TestPasswordLoginRejectedWhenPasskeyOnly(t *testing.T) {
	av := newTestAviary(t)
	cookie := loginAs(t, av, "admin@example.com", "supersecret")
	enrollSuperuserPasskey(t, av, "cred-1")

	rec := doControl(t, av, http.MethodPut, "/api/auth/security",
		securityRequest{PasswordLoginDisabled: true}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: status %d body %s", rec.Code, rec.Body.String())
	}

	// Session reflects the new state.
	rec = doControl(t, av, http.MethodGet, "/api/auth/session", nil)
	var sess struct {
		HasPasskeys           bool `json:"hasPasskeys"`
		PasswordLoginDisabled bool `json:"passwordLoginDisabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if !sess.HasPasskeys || !sess.PasswordLoginDisabled {
		t.Fatalf("session = %+v, want hasPasskeys+passwordLoginDisabled true", sess)
	}

	// Correct password is now refused with 403.
	rec = doControl(t, av, http.MethodPost, "/api/auth/login",
		loginRequest{Email: "admin@example.com", Password: "supersecret"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("password login while disabled: status %d body %s", rec.Code, rec.Body.String())
	}

	// Re-enabling password login restores normal sign-in.
	rec = doControl(t, av, http.MethodPut, "/api/auth/security",
		securityRequest{PasswordLoginDisabled: false}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable flag: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = doControl(t, av, http.MethodPost, "/api/auth/login",
		loginRequest{Email: "admin@example.com", Password: "supersecret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("password login after re-enable: status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPasskeyOnlyLockoutSafeguard(t *testing.T) {
	av := newTestAviary(t)
	cookie := loginAs(t, av, "admin@example.com", "supersecret")
	enrollSuperuserPasskey(t, av, "cred-1")

	if rec := doControl(t, av, http.MethodPut, "/api/auth/security",
		securityRequest{PasswordLoginDisabled: true}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("enable: status %d body %s", rec.Code, rec.Body.String())
	}

	// Deleting the last passkey auto-clears the flag so the operator is never
	// locked out, and password login works again.
	if rec := doControl(t, av, http.MethodDelete, "/api/auth/passkey/cred-1", nil, cookie); rec.Code != http.StatusOK {
		t.Fatalf("delete passkey: status %d body %s", rec.Code, rec.Body.String())
	}
	if got, _ := av.store.PasswordLoginDisabled(context.Background()); got {
		t.Fatal("flag should be auto-cleared after deleting the last passkey")
	}
	if rec := doControl(t, av, http.MethodPost, "/api/auth/login",
		loginRequest{Email: "admin@example.com", Password: "supersecret"}); rec.Code != http.StatusOK {
		t.Fatalf("password login after last-passkey delete: status %d body %s", rec.Code, rec.Body.String())
	}
}
