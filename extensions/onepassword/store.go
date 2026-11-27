package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	op "github.com/1password/onepassword-sdk-go"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type storeKey struct {
	priv             *ecdh.PrivateKey
	target           wire.SecretStoreTarget
	machine, session string
	until            time.Time
}
type storeValue struct {
	Name  string `json:"name"`
	Value []byte `json:"value"`
}

func storeFailure() error {
	return &ext.Error{Code: codeFailed, Message: "1Password storage failed; inspect item metadata before retrying"}
}
func storeInfo(key string) []byte { return []byte("persona secret store v1\x00" + key) }
func storeRandom() string {
	var b [24]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("random source unavailable")
	}
	return hex.EncodeToString(b[:])
}
func decodeStore(raw []byte, v any) error {
	if len(raw) > wire.SecretStoreSealedMost*2 {
		return storeFailure()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return storeFailure()
	}
	return nil
}

func cleanBase32Secret(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == ' ' || c == '\t' || c == '-' || c == '\r' || c == '\n' {
			continue
		}
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		b.WriteByte(c)
	}
	s := strings.TrimRight(b.String(), "=")
	if s == "" {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'A' && c <= 'Z') || (c >= '2' && c <= '7')) {
			return ""
		}
	}
	return s
}

// normalizeOTPValue accepts an otpauth:// URI or a raw base32 secret and returns the otpauth URI
// expected by 1Password CLI v2 native OTP fields.
func normalizeOTPValue(raw, itemTitle, fieldName string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", storeFailure()
	}
	if strings.HasPrefix(strings.ToLower(s), "otpauth://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", storeFailure()
		}
		kind := strings.ToLower(u.Host)
		if kind != "totp" && kind != "hotp" {
			return "", storeFailure()
		}
		secret := cleanBase32Secret(u.Query().Get("secret"))
		if secret == "" {
			return "", storeFailure()
		}
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
		if err != nil || len(decoded) == 0 {
			return "", storeFailure()
		}
		if u.Scheme != "otpauth" {
			u.Scheme = "otpauth"
			return u.String(), nil
		}
		return s, nil
	}
	secret := cleanBase32Secret(s)
	if secret == "" {
		return "", storeFailure()
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil || len(decoded) == 0 {
		return "", storeFailure()
	}
	label := strings.TrimSpace(itemTitle)
	if label == "" {
		label = strings.TrimSpace(fieldName)
	}
	if label == "" {
		label = "OTP"
	}
	return "otpauth://totp/" + url.PathEscape(label) + "?secret=" + secret, nil
}

func builtinFieldSpec(category, fieldName, fieldType string) (id, label, purpose, typ string, ok bool) {
	if fieldType == "OTP" {
		return "", "", "", "", false
	}
	if strings.EqualFold(category, "LOGIN") && strings.EqualFold(fieldName, "username") {
		return "username", "username", "USERNAME", "STRING", true
	}
	if (strings.EqualFold(category, "LOGIN") || strings.EqualFold(category, "PASSWORD")) && strings.EqualFold(fieldName, "password") {
		return "password", "password", "PASSWORD", "CONCEALED", true
	}
	return "", "", "", "", false
}

// store relays only public metadata/ciphertext to the selected resident node. SDK/service-account writes
// deliberately do not fall back here: the source encrypted to a key held only by that node process.
func (a *app) store(ctx context.Context, raw json.RawMessage) (any, error) {
	var in wire.SecretStoreParams
	if decodeStore(raw, &in) != nil || in.Target.Validate() != nil || (in.Contribution != "" && in.Contribution != providerID) {
		return nil, storeFailure()
	}
	a.mu.Lock()
	machine, call := a.machine, a.nodes
	a.mu.Unlock()
	if machine == "" || call == nil {
		return nil, &ext.Error{Code: "refused", Message: "secret writes require the configured resident op node"}
	}
	var out wire.SecretStoreResult
	if call(ctx, machine, "op.store", in, &out) != nil {
		return nil, storeFailure()
	}
	if in.Phase == "commit" {
		a.forgetMeta()
	}
	return out, nil
}

// storeNode runs with n.mu held, the same mutex/process as all existing op reads.
func (n *nodeApp) storeNode(ctx context.Context, raw json.RawMessage) (any, error) {
	var in wire.SecretStoreParams
	if decodeStore(raw, &in) != nil || in.Target.Validate() != nil || in.Machine == "" || in.Session == "" {
		return nil, storeFailure()
	}
	switch in.Phase {
	case "describe":
		if in.KeyID != "" || len(in.Sealed) != 0 {
			return nil, storeFailure()
		}
		t, vault, err := n.describeStore(ctx, in.Target)
		return wire.SecretStoreResult{Target: t, VaultName: vault}, err
	case "prepare":
		if in.KeyID != "" || len(in.Sealed) != 0 || !opID.MatchString(in.Target.Vault) || (in.Target.Operation == "edit" && !opID.MatchString(in.Target.Item)) {
			return nil, storeFailure()
		}
		if n.storeKeys == nil {
			n.storeKeys = map[string]storeKey{}
		}
		for id, k := range n.storeKeys {
			if time.Now().After(k.until) {
				delete(n.storeKeys, id)
			}
		}
		if len(n.storeKeys) >= 128 {
			return nil, storeFailure()
		}
		priv, err := ecdh.P256().GenerateKey(rand.Reader)
		if err != nil {
			return nil, storeFailure()
		}
		id := storeRandom()
		n.storeKeys[id] = storeKey{priv, in.Target, in.Machine, in.Session, time.Now().Add(2 * time.Minute)}
		return wire.SecretStoreResult{Target: in.Target, Key: base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), KeyID: id}, nil
	case "commit":
		k, ok := n.storeKeys[in.KeyID]
		if !ok || k.machine != in.Machine || k.session != in.Session {
			return nil, storeFailure()
		}
		delete(n.storeKeys, in.KeyID) // every owner attempt consumes it, including malformed ciphertext
		want, _ := json.Marshal(k.target)
		got, _ := json.Marshal(in.Target)
		if time.Now().After(k.until) || !bytes.Equal(want, got) || len(in.Sealed) == 0 || len(in.Sealed) > wire.SecretStoreSealedMost {
			return nil, storeFailure()
		}
		sk, err := hpke.NewDHKEMPrivateKey(k.priv)
		if err != nil {
			return nil, storeFailure()
		}
		sealed, err := base64.StdEncoding.DecodeString(in.Sealed)
		if err != nil {
			return nil, storeFailure()
		}
		defer clear(sealed)
		plain, err := hpke.Open(sk, hpke.HKDFSHA256(), hpke.AES256GCM(), storeInfo(in.KeyID), sealed)
		if err != nil {
			return nil, storeFailure()
		}
		defer clear(plain)
		var vals []storeValue
		defer func() {
			for _, v := range vals {
				clear(v.Value)
			}
		}()
		var valueFields []wire.SecretStoreField
		for _, f := range k.target.Fields {
			if !f.Delete && !f.KeepValue {
				valueFields = append(valueFields, f)
			}
		}
		if decodeStore(plain, &vals) != nil || len(vals) != len(valueFields) {
			return nil, storeFailure()
		}
		for i, v := range vals {
			if v.Name != valueFields[i].Name || len(v.Value) < 1 || len(v.Value) > wire.SecretStoreValueMost || !utf8.Valid(v.Value) || bytes.IndexByte(v.Value, 0) >= 0 {
				return nil, storeFailure()
			}
		}
		return n.writeStore(ctx, k.target, vals)
	}
	return nil, storeFailure()
}
func (n *nodeApp) describeStore(ctx context.Context, t wire.SecretStoreTarget) (wire.SecretStoreTarget, string, error) {
	vaults, err := n.vaults(ctx)
	if err != nil {
		return t, "", storeFailure()
	}
	v, ok := pick(vaults, t.Vault, func(v op.VaultOverview) (string, string) { return v.ID, v.Title })
	if !ok {
		return t, "", storeFailure()
	}
	t.Vault = v.ID
	if t.Operation == "edit" {
		if !opID.MatchString(t.Item) {
			return t, "", storeFailure()
		}
		it, err := n.item(ctx, t.Vault, t.Item)
		if err != nil {
			return t, "", storeFailure()
		}
		t.Title = it.Title
	}
	return t, v.Title, nil
}

// Full templates stay inside the node. Raw JSON preserves unrelated fields, sections, URLs and version.
// Edits refuse items carrying passkeys or attachments because CLI JSON templates cannot safely preserve them.
func (n *nodeApp) writeStore(ctx context.Context, t wire.SecretStoreTarget, vals []storeValue) (any, error) {
	item := map[string]json.RawMessage{}
	put := func(m map[string]json.RawMessage, k string, v any) { m[k], _ = json.Marshal(v) }
	var fields []map[string]json.RawMessage
	category := t.Category
	title := t.Title
	args := []string{"item", "create", "-", "--vault", t.Vault, "--format", "json", "--reveal"}
	if t.Operation == "edit" {
		raw, err := n.run(ctx, "item", "get", t.Item, "--vault", t.Vault, "--format", "json", "--reveal")
		if err != nil {
			return nil, storeFailure()
		}
		defer clear(raw)
		if json.Unmarshal(raw, &item) != nil {
			return nil, storeFailure()
		}
		var gotTitle, gotCategory, id string
		_ = json.Unmarshal(item["title"], &gotTitle)
		_ = json.Unmarshal(item["category"], &gotCategory)
		_ = json.Unmarshal(item["id"], &id)
		if gotTitle != t.Title || id != t.Item {
			return nil, &ext.Error{Code: "refused", Message: "The item changed before commit"}
		}
		title = gotTitle
		category = gotCategory
		for _, key := range []string{"passkeys", "files", "document"} {
			if b := bytes.TrimSpace(item[key]); len(b) > 0 && string(b) != "null" && string(b) != "[]" {
				return nil, &ext.Error{Code: "refused", Message: "Template writes cannot preserve this item's passkeys or attachments"}
			}
		}
		if json.Unmarshal(item["fields"], &fields) != nil {
			return nil, storeFailure()
		}
		for _, f := range fields {
			var ftype string
			_ = json.Unmarshal(f["type"], &ftype)
			if strings.EqualFold(ftype, "PASSKEY") {
				return nil, &ext.Error{Code: "refused", Message: "Template writes cannot preserve this item's passkeys or attachments"}
			}
		}
		args = []string{"item", "edit", t.Item, "--vault", t.Vault, "--format", "json", "--reveal"}
	} else {
		put(item, "title", t.Title)
		put(item, "category", t.Category)
		put(item, "vault", map[string]string{"id": t.Vault})
	}
	// Clear all owned encodings even on failure. Go strings/encoder copies remain subject to GC.
	defer func() {
		for _, b := range item {
			clear(b)
		}
		for _, f := range fields {
			for _, b := range f {
				clear(b)
			}
		}
	}()
	// The CLI may attach ordinary fields to an unnamed default section. That is not a
	// user-visible section (the metadata reader also treats it as unsectioned).
	var sections []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if raw := item["sections"]; len(raw) > 0 && json.Unmarshal(raw, &sections) != nil {
		return nil, storeFailure()
	}
	unnamed := map[string]bool{}
	for _, sec := range sections {
		if strings.TrimSpace(sec.Label) == "" {
			unnamed[sec.ID] = true
		}
	}
	checkSection := func(f map[string]json.RawMessage) error {
		if raw := bytes.TrimSpace(f["section"]); len(raw) > 0 && string(raw) != "null" {
			var section struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &section) != nil || (section.ID != "" && !unnamed[section.ID]) {
				return &ext.Error{Code: "refused", Message: "Named or unknown sections are not supported for storage yet"}
			}
		}
		return nil
	}

	// Process deletions first so removing a duplicate field in the same edit clears label ambiguity.
	deletedIDs := map[string]string{}
	for _, tf := range t.Fields {
		if !tf.Delete {
			continue
		}
		match := -1
		for j, f := range fields {
			var label, id string
			_ = json.Unmarshal(f["label"], &label)
			_ = json.Unmarshal(f["id"], &id)
			if label == tf.Name || id == tf.Name {
				if match >= 0 {
					return nil, storeFailure()
				}
				match = j
			}
		}
		if match < 0 {
			return nil, storeFailure()
		}
		if err := checkSection(fields[match]); err != nil {
			return nil, err
		}
		var delID string
		_ = json.Unmarshal(fields[match]["id"], &delID)
		deletedIDs[tf.Name] = delID
		for _, b := range fields[match] {
			clear(b)
		}
		fields = append(fields[:match], fields[match+1:]...)
	}

	writtenID := map[string]string{}
	writtenPurpose := map[string]string{}
	valIdx := 0
	for _, tf := range t.Fields {
		if tf.Delete {
			continue
		}
		match := -1
		if bid, _, bpurpose, _, ok := builtinFieldSpec(category, tf.Name, tf.Type); ok {
			for j, f := range fields {
				var id, purpose string
				_ = json.Unmarshal(f["id"], &id)
				_ = json.Unmarshal(f["purpose"], &purpose)
				if id == bid || (bpurpose != "" && strings.EqualFold(purpose, bpurpose)) {
					if match >= 0 {
						return nil, storeFailure()
					}
					match = j
				}
			}
		}
		if match < 0 {
			for j, f := range fields {
				var label, id string
				_ = json.Unmarshal(f["label"], &label)
				_ = json.Unmarshal(f["id"], &id)
				if label == tf.Name || id == tf.Name {
					if match >= 0 {
						return nil, storeFailure()
					}
					match = j
				}
			}
		}
		if tf.KeepValue {
			if match < 0 {
				return nil, storeFailure()
			}
			f := fields[match]
			if err := checkSection(f); err != nil {
				return nil, err
			}
			var existing string
			if json.Unmarshal(f["value"], &existing) != nil || existing == "" {
				return nil, storeFailure()
			}
			nextVal := existing
			if tf.Type == "OTP" {
				norm, err := normalizeOTPValue(existing, title, tf.Name)
				if err != nil {
					return nil, err
				}
				nextVal = norm
				delete(f, "totp")
			}
			clear(f["value"])
			put(f, "value", nextVal)
			put(f, "type", tf.Type)
			var fid string
			_ = json.Unmarshal(f["id"], &fid)
			writtenID[tf.Name] = fid
			continue
		}
		if valIdx >= len(vals) {
			return nil, storeFailure()
		}
		v := vals[valIdx]
		valIdx++
		nextVal := string(v.Value)
		if tf.Type == "OTP" {
			norm, err := normalizeOTPValue(nextVal, title, tf.Name)
			if err != nil {
				return nil, err
			}
			nextVal = norm
		}
		if match < 0 {
			nf := map[string]json.RawMessage{}
			if bid, blabel, bpurpose, btype, ok := builtinFieldSpec(category, tf.Name, tf.Type); ok {
				put(nf, "id", bid)
				put(nf, "label", blabel)
				put(nf, "purpose", bpurpose)
				put(nf, "type", btype)
				writtenID[tf.Name] = bid
				writtenPurpose[tf.Name] = bpurpose
			} else {
				newID := storeRandom()[:26]
				put(nf, "id", newID)
				put(nf, "label", tf.Name)
				put(nf, "type", tf.Type)
				writtenID[tf.Name] = newID
			}
			put(nf, "value", nextVal)
			fields = append(fields, nf)
			continue
		}
		f := fields[match]
		if err := checkSection(f); err != nil {
			return nil, err
		}
		var fid, fpurpose string
		_ = json.Unmarshal(f["id"], &fid)
		_ = json.Unmarshal(f["purpose"], &fpurpose)
		if tf.Type == "OTP" {
			delete(f, "totp")
		}
		clear(f["value"])
		put(f, "value", nextVal)
		typ := tf.Type
		if bid, _, bpurpose, btype, ok := builtinFieldSpec(category, tf.Name, tf.Type); ok && (fid == bid || strings.EqualFold(fpurpose, bpurpose)) {
			typ = btype
			writtenPurpose[tf.Name] = bpurpose
		}
		put(f, "type", typ)
		writtenID[tf.Name] = fid
	}
	clear(item["fields"])
	put(item, "fields", fields)
	template, err := json.Marshal(item)
	if err != nil {
		return nil, storeFailure()
	}
	defer clear(template)
	run := n.write
	if run == nil {
		run = runOpInput
	}
	out, err := run(ctx, template, args...)
	if err != nil {
		return nil, storeFailure()
	}
	defer clear(out)
	n.noteAuthorization(time.Now())
	var saved cliItem
	defer func() {
		for _, f := range saved.Fields {
			clear(f.Value)
		}
	}()
	if json.Unmarshal(out, &saved) != nil || !opID.MatchString(saved.ID) || saved.Vault.ID != t.Vault {
		return nil, storeFailure()
	}
	for _, delID := range deletedIDs {
		if delID == "" {
			continue
		}
		for _, sf := range saved.Fields {
			if sf.ID == delID {
				return nil, storeFailure()
			}
		}
	}
	refs := map[string]string{}
	for _, f := range t.Fields {
		if f.Delete {
			continue
		}
		id := ""
		if wantID := writtenID[f.Name]; wantID != "" {
			for _, sf := range saved.Fields {
				if sf.ID == wantID {
					id = sf.ID
					break
				}
			}
		}
		if id == "" {
			if wantPurpose := writtenPurpose[f.Name]; wantPurpose != "" {
				for _, sf := range saved.Fields {
					if strings.EqualFold(sf.Purpose, wantPurpose) {
						if id != "" {
							return nil, storeFailure()
						}
						id = sf.ID
					}
				}
			}
		}
		if id == "" {
			for _, sf := range saved.Fields {
				if sf.Label == f.Name || sf.ID == f.Name {
					if id != "" {
						return nil, storeFailure()
					}
					id = sf.ID
				}
			}
		}
		if id == "" || !refSafe(id) {
			return nil, storeFailure()
		}
		refs[f.Name] = "op://" + saved.Vault.ID + "/" + saved.ID + "/" + id
	}
	return wire.SecretStoreResult{Target: t, References: refs}, nil
}
