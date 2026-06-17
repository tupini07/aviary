package passkey

import (
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pocketbase/pocketbase/core"
)

// passkeyUser adapts a PocketBase auth record to the webauthn.User interface.
// The WebAuthn user handle is the record id, so a discoverable login can map a
// returned user handle straight back to its PocketBase record.
type passkeyUser struct {
	record *core.Record
	creds  []webauthn.Credential
}

// newUser loads the user's stored credentials and returns a webauthn.User.
func newUser(app core.App, record *core.Record) (*passkeyUser, error) {
	creds, err := loadCredentials(app, record.Id)
	if err != nil {
		return nil, err
	}
	return &passkeyUser{record: record, creds: creds}, nil
}

func (u *passkeyUser) WebAuthnID() []byte { return []byte(u.record.Id) }

func (u *passkeyUser) WebAuthnName() string {
	if email := u.record.Email(); email != "" {
		return email
	}
	return u.record.Id
}

func (u *passkeyUser) WebAuthnDisplayName() string {
	if name := u.record.GetString("name"); name != "" {
		return name
	}
	return u.WebAuthnName()
}

func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// WebAuthnIcon is deprecated in the spec but still part of the interface in some
// versions; returning an empty string is the documented no-op.
func (u *passkeyUser) WebAuthnIcon() string { return "" }
