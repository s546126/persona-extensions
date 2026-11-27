package main

import (
	"context"
	"crypto/ecdh"
	"crypto/hpke"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

func storeCall(t *testing.T, n *nodeApp, in wire.SecretStoreParams) (wire.SecretStoreResult, error) {
	t.Helper()
	b, _ := json.Marshal(in)
	got, err := n.handle(context.Background(), "store", b)
	if err != nil {
		return wire.SecretStoreResult{}, err
	}
	raw, _ := json.Marshal(got)
	var out wire.SecretStoreResult
	err = json.Unmarshal(raw, &out)
	return out, err
}
func storeSeal(t *testing.T, key wire.SecretStoreResult, values []storeValue) string {
	t.Helper()
	raw, _ := base64.StdEncoding.DecodeString(key.Key)
	pub, _ := ecdh.P256().NewPublicKey(raw)
	pk, _ := hpke.NewDHKEMPublicKey(pub)
	plain, _ := json.Marshal(values)
	defer clear(plain)
	out, err := hpke.Seal(pk, hpke.HKDFSHA256(), hpke.AES256GCM(), storeInfo(key.KeyID), plain)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(out)
}
func storeTarget() wire.SecretStoreTarget {
	return wire.SecretStoreTarget{Operation: "create", Vault: fixVault, Title: "Synthetic test", Category: "API_CREDENTIAL", Fields: []wire.SecretStoreField{{Name: "credential", Type: "CONCEALED"}}}
}
func TestStorageCreateEditOnlyStdinAndReferenceAnswers(t *testing.T) {
	const secret = "FAKE-WRITE-VALUE-98181"
	n := &nodeApp{}
	var stored map[string]any
	writes := 0
	n.write = func(_ context.Context, input []byte, args ...string) ([]byte, error) {
		writes++
		if strings.Contains(strings.Join(args, " "), secret) {
			t.Fatal("value in argv")
		}
		if json.Unmarshal(input, &stored) != nil {
			t.Fatal("invalid stdin template")
		}
		fs := stored["fields"].([]any)
		if fs[len(fs)-1].(map[string]any)["value"] != secret {
			t.Fatal("stdin did not contain exact field value")
		}
		stored["id"] = fixItem
		stored["vault"] = map[string]string{"id": fixVault}
		return json.Marshal(stored)
	}
	in := wire.SecretStoreParams{Phase: "prepare", Target: storeTarget(), Machine: "source", Session: "writer"}
	key, err := storeCall(t, n, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Phase = "commit"
	in.KeyID = key.KeyID
	in.Sealed = storeSeal(t, key, []storeValue{{Name: "credential", Value: []byte(secret)}})
	out, err := storeCall(t, n, in)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(out)
	if strings.Contains(string(public), secret) || !strings.HasPrefix(out.References["credential"], "op://"+fixVault+"/"+fixItem+"/") {
		t.Fatal("unsafe result")
	}
	if _, err = storeCall(t, n, in); err == nil || writes != 1 {
		t.Fatal("replay wrote")
	}
	// Edit preserves unrelated values, URLs and version entirely within the node,
	// including the CLI's unnamed default section used for ordinary custom fields.
	stored["fields"].([]any)[0].(map[string]any)["section"] = map[string]string{"id": "add more"}
	stored["sections"] = []any{map[string]any{"id": "add more", "label": ""}}
	stored["fields"] = append([]any{map[string]any{"id": "untouched", "label": "untouched", "type": "STRING", "value": "OTHER-FAKE-VALUE"}}, stored["fields"].([]any)...)
	stored["version"] = float64(7)
	stored["urls"] = []any{map[string]any{"href": "https://example.com"}}
	before, _ := json.Marshal(stored)
	n.run = func(context.Context, ...string) ([]byte, error) { return append([]byte{}, before...), nil }
	in = wire.SecretStoreParams{Phase: "prepare", Target: storeTarget(), Machine: "source", Session: "writer"}
	in.Target.Operation = "edit"
	in.Target.Item = fixItem
	in.Target.Category = ""
	key, err = storeCall(t, n, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Phase = "commit"
	in.KeyID = key.KeyID
	in.Sealed = storeSeal(t, key, []storeValue{{Name: "credential", Value: []byte(secret)}})
	if _, err = storeCall(t, n, in); err != nil {
		t.Fatal(err)
	}
	if stored["version"] != float64(7) || stored["fields"].([]any)[0].(map[string]any)["value"] != "OTHER-FAKE-VALUE" || len(stored["urls"].([]any)) != 1 {
		t.Fatal("unrelated metadata or fields lost")
	}
	stored["sections"].([]any)[0].(map[string]any)["label"] = "Named section"
	before, _ = json.Marshal(stored)
	if _, err = n.writeStore(context.Background(), in.Target, []storeValue{{Name: "credential", Value: []byte(secret)}}); err == nil || writes != 2 {
		t.Fatal("named section was silently edited")
	}

}
func TestStorageRejectsChangedExpiredForeignAndInvalidCiphertext(t *testing.T) {
	for _, change := range []string{"target", "session", "expired", "ciphertext", "field"} {
		t.Run(change, func(t *testing.T) {
			writes := 0
			n := &nodeApp{write: func(context.Context, []byte, ...string) ([]byte, error) {
				writes++
				return nil, errors.New("must never run")
			}}
			in := wire.SecretStoreParams{Phase: "prepare", Target: storeTarget(), Machine: "source", Session: "writer"}
			key, err := storeCall(t, n, in)
			if err != nil {
				t.Fatal(err)
			}
			vals := []storeValue{{Name: "credential", Value: []byte("synthetic-payload")}}
			if change == "field" {
				vals[0].Name = "not-approved"
			}
			in.Phase = "commit"
			in.KeyID = key.KeyID
			in.Sealed = storeSeal(t, key, vals)
			switch change {
			case "target":
				in.Target.Title = "Different"
			case "session":
				in.Session = "other"
			case "expired":
				k := n.storeKeys[key.KeyID]
				k.until = time.Now().Add(-time.Second)
				n.storeKeys[key.KeyID] = k
			case "ciphertext":
				in.Sealed = "invalid"
			}
			if _, err = storeCall(t, n, in); err == nil || writes != 0 {
				t.Fatal("unapproved write accepted")
			}
		})
	}
}
func TestStorageHidesOpErrorsAndRefusesLossyPasskeyEdits(t *testing.T) {
	n := &nodeApp{write: func(context.Context, []byte, ...string) ([]byte, error) {
		return nil, errors.New("SYNTHETIC-SECRET-IN-STDERR")
	}}
	_, err := n.writeStore(context.Background(), storeTarget(), []storeValue{{Name: "credential", Value: []byte("fake")}})
	if err == nil || strings.Contains(err.Error(), "SYNTHETIC-SECRET") {
		t.Fatal("unsafe error")
	}
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{
			name: "passkey_field",
			raw:  `{"id":"` + fixItem + `","title":"Synthetic test","category":"LOGIN","fields":[{"id":"pk1","label":"passkey","type":"PASSKEY"}]}`,
		},
		{
			name: "passkeys_array",
			raw:  `{"id":"` + fixItem + `","title":"Synthetic test","category":"LOGIN","passkeys":[{"id":"pk1"}],"fields":[]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n.run = func(context.Context, ...string) ([]byte, error) { return []byte(tc.raw), nil }
			target := storeTarget()
			target.Operation = "edit"
			target.Item = fixItem
			target.Category = ""
			if _, err = n.writeStore(context.Background(), target, []storeValue{{Name: "credential", Value: []byte("fake")}}); err == nil {
				t.Fatal("potential passkey loss accepted")
			}
		})
	}
}

func TestStorageOTPBase32AndOtpauth(t *testing.T) {
	const (
		rawBase32 = "jbsw y3dp-ehpk 3pxp==\n"
		uriInput  = "otpauth://totp/Example:synthetic?secret=JBSWY3DPEHPK3PXP&issuer=Example"
	)
	n := &nodeApp{}
	var templates []map[string]any
	n.write = func(_ context.Context, input []byte, _ ...string) ([]byte, error) {
		var tpl map[string]any
		if err := json.Unmarshal(input, &tpl); err != nil {
			t.Fatal(err)
		}
		templates = append(templates, tpl)
		tpl["id"] = fixItem
		tpl["vault"] = map[string]string{"id": fixVault}
		return json.Marshal(tpl)
	}

	target := wire.SecretStoreTarget{
		Operation: "create",
		Vault:     fixVault,
		Title:     "Synthetic OTP",
		Category:  "LOGIN",
		Fields: []wire.SecretStoreField{
			{Name: "totp_base32", Type: "OTP"},
			{Name: "totp_uri", Type: "OTP"},
		},
	}
	in := wire.SecretStoreParams{Phase: "prepare", Target: target, Machine: "source", Session: "writer"}
	key, err := storeCall(t, n, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Phase = "commit"
	in.KeyID = key.KeyID
	in.Sealed = storeSeal(t, key, []storeValue{
		{Name: "totp_base32", Value: []byte(rawBase32)},
		{Name: "totp_uri", Value: []byte(uriInput)},
	})
	out, err := storeCall(t, n, in)
	if err != nil {
		t.Fatalf("otp create failed: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("expected 1 write, got %d", len(templates))
	}
	fs := templates[0]["fields"].([]any)
	if len(fs) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fs))
	}
	f0 := fs[0].(map[string]any)
	if f0["type"] != "OTP" || f0["value"] != "otpauth://totp/Synthetic%20OTP?secret=JBSWY3DPEHPK3PXP" {
		t.Fatalf("unexpected base32 OTP field: %+v", f0)
	}
	f1 := fs[1].(map[string]any)
	if f1["type"] != "OTP" || f1["value"] != uriInput {
		t.Fatalf("unexpected otpauth OTP field: %+v", f1)
	}
	if !strings.HasPrefix(out.References["totp_base32"], "op://"+fixVault+"/"+fixItem+"/") ||
		!strings.HasPrefix(out.References["totp_uri"], "op://"+fixVault+"/"+fixItem+"/") {
		t.Fatalf("missing OTP references: %+v", out.References)
	}

	// Invalid OTP values must be rejected without invoking op.
	for _, bad := range []string{"not-valid-base32!!!", "otpauth://totp/Example?issuer=Example", "https://example.com"} {
		if _, err := normalizeOTPValue(bad, "Synthetic", "totp"); err == nil {
			t.Fatalf("invalid OTP value accepted: %q", bad)
		}
	}
}

func TestStorageLoginCreateUsesBuiltinFieldsWithoutFalseError(t *testing.T) {
	n := &nodeApp{}
	var sent map[string]any
	n.write = func(_ context.Context, input []byte, _ ...string) ([]byte, error) {
		if err := json.Unmarshal(input, &sent); err != nil {
			t.Fatal(err)
		}
		// Simulate op item create returning the built-in LOGIN fields plus notesPlain and custom OTP field.
		fs := sent["fields"].([]any)
		returned := append([]any{}, fs...)
		returned = append(returned, map[string]any{"id": "notesPlain", "label": "notesPlain", "purpose": "NOTES", "type": "STRING", "value": ""})
		resp := map[string]any{
			"id":       fixItem,
			"title":    sent["title"],
			"category": "LOGIN",
			"vault":    map[string]string{"id": fixVault},
			"fields":   returned,
		}
		return json.Marshal(resp)
	}

	target := wire.SecretStoreTarget{
		Operation: "create",
		Vault:     fixVault,
		Title:     "Synthetic Login",
		Category:  "LOGIN",
		Fields: []wire.SecretStoreField{
			{Name: "username", Type: "STRING"},
			{Name: "password", Type: "CONCEALED"},
			{Name: "totp_secret", Type: "OTP"},
		},
	}
	in := wire.SecretStoreParams{Phase: "prepare", Target: target, Machine: "source", Session: "writer"}
	key, err := storeCall(t, n, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Phase = "commit"
	in.KeyID = key.KeyID
	in.Sealed = storeSeal(t, key, []storeValue{
		{Name: "username", Value: []byte("synthetic-user")},
		{Name: "password", Value: []byte("SYNTHETIC-PASS-12345")},
		{Name: "totp_secret", Value: []byte("JBSWY3DPEHPK3PXP")},
	})
	out, err := storeCall(t, n, in)
	if err != nil {
		t.Fatalf("LOGIN create falsely failed: %v", err)
	}
	fs := sent["fields"].([]any)
	if len(fs) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(fs))
	}
	u := fs[0].(map[string]any)
	p := fs[1].(map[string]any)
	if u["id"] != "username" || u["purpose"] != "USERNAME" || u["type"] != "STRING" {
		t.Fatalf("username did not use built-in LOGIN field: %+v", u)
	}
	if p["id"] != "password" || p["purpose"] != "PASSWORD" || p["type"] != "CONCEALED" {
		t.Fatalf("password did not use built-in LOGIN field: %+v", p)
	}
	if out.References["username"] != "op://"+fixVault+"/"+fixItem+"/username" ||
		out.References["password"] != "op://"+fixVault+"/"+fixItem+"/password" ||
		!strings.HasPrefix(out.References["totp_secret"], "op://"+fixVault+"/"+fixItem+"/") {
		t.Fatalf("unexpected LOGIN references: %+v", out.References)
	}
}

func TestStorageEditRetypeToOTPAndDeleteDuplicateFields(t *testing.T) {
	existing := map[string]any{
		"id":       fixItem,
		"title":    "Synthetic Login",
		"category": "LOGIN",
		"vault":    map[string]string{"id": fixVault},
		"version":  float64(3),
		"fields": []any{
			map[string]any{"id": "username", "label": "username", "purpose": "USERNAME", "type": "STRING", "value": ""},
			map[string]any{"id": "password", "label": "password", "purpose": "PASSWORD", "type": "CONCEALED", "value": ""},
			map[string]any{"id": "dupuser1234567890123456789", "label": "username", "type": "STRING", "value": "old-dup-user"},
			map[string]any{"id": "duppass1234567890123456789", "label": "password", "type": "CONCEALED", "value": "old-dup-pass"},
			map[string]any{"id": "totpid12345678901234567890", "label": "totp_secret", "type": "CONCEALED", "value": "JBSWY3DPEHPK3PXP"},
		},
	}
	before, _ := json.Marshal(existing)
	var written map[string]any
	n := &nodeApp{
		run: func(context.Context, ...string) ([]byte, error) {
			return append([]byte{}, before...), nil
		},
		write: func(_ context.Context, input []byte, _ ...string) ([]byte, error) {
			if err := json.Unmarshal(input, &written); err != nil {
				t.Fatal(err)
			}
			return input, nil
		},
	}

	target := wire.SecretStoreTarget{
		Operation: "edit",
		Vault:     fixVault,
		Item:      fixItem,
		Title:     "Synthetic Login",
		Fields: []wire.SecretStoreField{
			{Name: "dupuser1234567890123456789", Delete: true},
			{Name: "duppass1234567890123456789", Delete: true},
			{Name: "username", Type: "STRING"},
			{Name: "totp_secret", Type: "OTP", KeepValue: true},
		},
	}
	in := wire.SecretStoreParams{Phase: "prepare", Target: target, Machine: "source", Session: "writer"}
	key, err := storeCall(t, n, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Phase = "commit"
	in.KeyID = key.KeyID
	in.Sealed = storeSeal(t, key, []storeValue{{Name: "username", Value: []byte("synthetic-user")}})
	out, err := storeCall(t, n, in)
	if err != nil {
		t.Fatalf("edit retype+delete failed: %v", err)
	}
	fs := written["fields"].([]any)
	if len(fs) != 3 {
		t.Fatalf("expected 3 remaining fields after deleting 2 duplicates, got %d: %+v", len(fs), fs)
	}
	var gotTotp map[string]any
	for _, raw := range fs {
		f := raw.(map[string]any)
		if f["id"] == "dupuser1234567890123456789" || f["id"] == "duppass1234567890123456789" {
			t.Fatalf("deleted duplicate field still present: %+v", f)
		}
		if f["id"] == "totpid12345678901234567890" {
			gotTotp = f
		}
	}
	if gotTotp == nil || gotTotp["type"] != "OTP" || gotTotp["value"] != "otpauth://totp/Synthetic%20Login?secret=JBSWY3DPEHPK3PXP" {
		t.Fatalf("totp_secret was not converted to native OTP: %+v", gotTotp)
	}
	if out.References["username"] != "op://"+fixVault+"/"+fixItem+"/username" ||
		out.References["totp_secret"] != "op://"+fixVault+"/"+fixItem+"/totpid12345678901234567890" {
		t.Fatalf("unexpected edit references: %+v", out.References)
	}
	if _, hasDeleted := out.References["dupuser1234567890123456789"]; hasDeleted {
		t.Fatalf("deleted field should not appear in references: %+v", out.References)
	}
}
func TestKeepaliveStopsAtHardLimitAndAfterFailure(t *testing.T) {
	n := &nodeApp{}
	now := time.Now()
	if n.keepaliveDue(now) {
		t.Fatal("unauthorized session kept alive")
	}
	n.noteAuthorization(now)
	if n.keepaliveDue(now.Add(4*time.Minute)) || !n.keepaliveDue(now.Add(6*time.Minute)) || n.keepaliveDue(now.Add(12*time.Hour)) {
		t.Fatal("wrong idle/hard boundaries")
	}
	n.keepaliveBlocked = true
	if n.keepaliveDue(now.Add(6 * time.Minute)) {
		t.Fatal("failed session repeatedly prompted")
	}
	n.noteAuthorization(now.Add(13 * time.Hour))
	if !n.keepaliveDue(now.Add(13*time.Hour + 6*time.Minute)) {
		t.Fatal("foreground reauthorization did not restart keepalive")
	}
}
