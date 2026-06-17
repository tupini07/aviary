package passkey

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// EnsureCollection creates the passkey credentials collection if it does not
// already exist. The collection is API-locked (no public rules); all access
// goes through the ceremony endpoints. It is safe to call repeatedly.
func EnsureCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(CollectionName); err == nil {
		return nil // already exists
	}

	col := core.NewBaseCollection(CollectionName)
	col.Fields.Add(
		&core.TextField{Name: "userId", Required: true},
		&core.TextField{Name: "credentialId", Required: true},
		&core.TextField{Name: "label"},
		&core.JSONField{Name: "data", MaxSize: 1 << 16},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	col.AddIndex("idx_passkeys_user", false, "userId", "")
	col.AddIndex("idx_passkeys_credential", true, "credentialId", "")

	if err := app.Save(col); err != nil {
		return fmt.Errorf("passkey: create collection: %w", err)
	}
	return nil
}

// encodeID returns the base64url (no padding) form of a credential id, used as
// the stable lookup key in the credentialId column.
func encodeID(id []byte) string {
	return base64.RawURLEncoding.EncodeToString(id)
}

// loadCredentials returns all stored WebAuthn credentials owned by userId.
func loadCredentials(app core.App, userId string) ([]webauthn.Credential, error) {
	records, err := app.FindAllRecords(CollectionName, dbx.HashExp{"userId": userId})
	if err != nil {
		return nil, fmt.Errorf("passkey: load credentials: %w", err)
	}
	creds := make([]webauthn.Credential, 0, len(records))
	for _, rec := range records {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(rec.GetString("data")), &c); err != nil {
			return nil, fmt.Errorf("passkey: decode credential %s: %w", rec.Id, err)
		}
		creds = append(creds, c)
	}
	return creds, nil
}

// saveCredential persists a freshly-registered credential for userId.
func saveCredential(app core.App, userId, label string, cred *webauthn.Credential) error {
	col, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return err
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("passkey: encode credential: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("userId", userId)
	rec.Set("credentialId", encodeID(cred.ID))
	rec.Set("label", label)
	rec.Set("data", string(data))
	if err := app.Save(rec); err != nil {
		return fmt.Errorf("passkey: save credential: %w", err)
	}
	return nil
}

// updateCredential persists changes to an existing credential (e.g. its signature
// counter after a successful login). It is a no-op if the credential is unknown.
func updateCredential(app core.App, userId string, cred *webauthn.Credential) error {
	rec, err := app.FindFirstRecordByFilter(CollectionName,
		"credentialId = {:cid} && userId = {:uid}",
		dbx.Params{"cid": encodeID(cred.ID), "uid": userId})
	if err != nil {
		return nil // unknown credential; nothing to update
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("passkey: encode credential: %w", err)
	}
	rec.Set("data", string(data))
	if err := app.Save(rec); err != nil {
		return fmt.Errorf("passkey: update credential: %w", err)
	}
	return nil
}

// hasCredentials reports whether userId owns at least one passkey.
func hasCredentials(app core.App, userId string) (bool, error) {
	records, err := app.FindAllRecords(CollectionName, dbx.HashExp{"userId": userId})
	if err != nil {
		return false, err
	}
	return len(records) > 0, nil
}

// credentialInfo is the public, non-sensitive view of a stored passkey. The
// WebAuthn material in the "data" column is deliberately never exposed.
type credentialInfo struct {
	CredentialID string `json:"credentialId"`
	Label        string `json:"label"`
	Created      string `json:"created"`
}

// listUserPasskeys returns the non-sensitive metadata for every passkey owned
// by userId, so a user can review their enrolled credentials.
func listUserPasskeys(app core.App, userId string) ([]credentialInfo, error) {
	records, err := app.FindAllRecords(CollectionName, dbx.HashExp{"userId": userId})
	if err != nil {
		return nil, fmt.Errorf("passkey: list credentials: %w", err)
	}
	out := make([]credentialInfo, 0, len(records))
	for _, rec := range records {
		out = append(out, credentialInfo{
			CredentialID: rec.GetString("credentialId"),
			Label:        rec.GetString("label"),
			Created:      rec.GetString("created"),
		})
	}
	return out, nil
}

// deleteUserPasskey removes a single passkey owned by userId, identified by its
// credentialId. Scoping the lookup to userId ensures a user can only delete
// their own credentials. It reports whether a matching credential was found.
func deleteUserPasskey(app core.App, userId, credentialId string) (bool, error) {
	rec, err := app.FindFirstRecordByFilter(CollectionName,
		"credentialId = {:cid} && userId = {:uid}",
		dbx.Params{"cid": credentialId, "uid": userId})
	if err != nil {
		return false, nil // not found (or not owned by this user)
	}
	if err := app.Delete(rec); err != nil {
		return false, fmt.Errorf("passkey: delete credential: %w", err)
	}
	return true, nil
}
