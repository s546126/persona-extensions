package main

// Only the metadata types in cache.go go into the connector's existing Hub-sealed secret slot. No field value,
// account token, CLI session or error is part of this record. Its private name-index seed is sealed with it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

const (
	metadataFormat   = "onepassword.metadata"
	metadataVersion  = 1
	maxMetadataBytes = 512 * 1024
	metadataTimeout  = 3 * time.Second
)

var errMetadataStore = errors.New("the Hub could not keep 1Password metadata securely; try again")

type metadataStore interface {
	Load(context.Context) ([]byte, error)
	Save(context.Context, []byte) error
	Delete(context.Context) error
}

type hubMetadataStore struct{ hub *ext.Hub }

func (s hubMetadataStore) Load(ctx context.Context) ([]byte, error) {
	return s.hub.LoadSecret(ctx, connectorID)
}
func (s hubMetadataStore) Save(ctx context.Context, b []byte) error {
	return s.hub.SaveSecret(ctx, connectorID, b)
}
func (s hubMetadataStore) Delete(ctx context.Context) error {
	return s.hub.DeleteSecret(ctx, connectorID)
}

type metadataEntry[T any] struct {
	Value T         `json:"value"`
	At    time.Time `json:"at"`
}

type metadataRecord struct {
	Format  string                               `json:"format"`
	Version int                                  `json:"version"`
	Source  string                               `json:"source"`
	Seed    []byte                               `json:"name_seed"`
	Vaults  *metadataEntry[[]vaultInfo]          `json:"vaults,omitempty"`
	Items   map[string]metadataEntry[[]itemInfo] `json:"items,omitempty"`
	Details map[string]metadataEntry[itemDetail] `json:"details,omitempty"`
}

// A different computer or service-account token must not receive a previous source's metadata. An unused token
// does not change the identity of the selected computer. Neither the machine ID nor the token is stored here.
func metadataSource(machine, token string) string {
	source := "token\x00" + token
	if machine != "" {
		source = "machine\x00" + machine
	}
	sum := sha256.Sum256([]byte(metadataFormat + "\x00" + source))
	return hex.EncodeToString(sum[:])
}

// initialize must return before a Hub callback can finish. Reads wait for this load before deciding whether a
// provider ask is cold (may authorize) or quiet. A failed load never overwrites a possibly valid sealed answer.
func (a *app) startMeta(ctx context.Context, store metadataStore, source string) {
	a.metaIO.Lock()
	a.metaMu.Lock()
	a.metaGen++
	gen := a.metaGen
	a.meta, a.metaRev, a.metaSaved = metaCache{}, 0, 0
	a.metaSeed = newNameSecret()
	a.metaStore, a.metaSource, a.metaLoadErr = store, source, nil
	loaded := make(chan struct{})
	a.metaLoaded = loaded
	a.metaMu.Unlock()
	a.metaIO.Unlock()
	go a.loadMeta(ctx, store, source, gen, loaded)
}

func (a *app) loadMeta(ctx context.Context, store metadataStore, source string, gen uint64, loaded chan struct{}) {
	defer close(loaded)
	a.metaIO.Lock()
	defer a.metaIO.Unlock()
	a.metaMu.Lock()
	current := a.metaGen == gen
	a.metaMu.Unlock()
	if !current || store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	b, err := store.Load(ctx)
	var record metadataRecord
	if err == nil && len(b) > 0 {
		record, err = decodeMetadata(b, source, a.now())
		if err != nil {
			// Corrupt, obsolete or from a different source: discard, never reinterpret it as current metadata.
			err = store.Delete(ctx)
			b = nil
		}
	}
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	if a.metaGen != gen {
		return
	}
	if err != nil {
		a.metaLoadErr = errMetadataStore
		return
	}
	if len(b) == 0 {
		return
	}
	copy(a.metaSeed[:], record.Seed)
	if record.Vaults != nil {
		a.meta.vaults = &kept[[]vaultInfo]{v: record.Vaults.Value, at: record.Vaults.At}
	}
	a.meta.items = map[string]kept[[]itemInfo]{}
	for key, e := range record.Items {
		a.meta.items[key] = kept[[]itemInfo]{v: e.Value, at: e.At}
	}
	a.meta.details = map[string]kept[itemDetail]{}
	for key, e := range record.Details {
		a.meta.details[key] = kept[itemDetail]{v: e.Value, at: e.At}
	}
	a.metaRev, a.metaSaved = 1, 1
}

func (a *app) waitMeta(ctx context.Context) error {
	for {
		a.metaMu.Lock()
		loaded := a.metaLoaded
		a.metaMu.Unlock()
		if loaded == nil {
			return nil
		} // apps without a Hub, including the isolated provider tests
		select {
		case <-loaded:
		case <-ctx.Done():
			return ctx.Err()
		}
		a.metaMu.Lock()
		current, err := loaded == a.metaLoaded, a.metaLoadErr
		if current && err != nil {
			// Return this failure, then let the next caller await one new storage-only attempt. An unread sealed
			// answer must never become a cold provider ask; a temporary Hub failure must not last until restart.
			next := make(chan struct{})
			a.metaLoaded, a.metaLoadErr = next, nil
			store, source, gen := a.metaStore, a.metaSource, a.metaGen
			go a.loadMeta(context.Background(), store, source, gen, next)
		}
		a.metaMu.Unlock()
		if current {
			return err
		}
	}
}

func decodeMetadata(b []byte, source string, now time.Time) (metadataRecord, error) {
	var r metadataRecord
	bad := errors.New("invalid metadata record")
	if len(b) > maxMetadataBytes {
		return r, bad
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil {
		return r, bad
	}
	var extra any
	if d.Decode(&extra) != io.EOF || r.Format != metadataFormat || r.Version != metadataVersion || r.Source != source || len(r.Seed) != 32 || len(r.Items) > maxMetaCacheEntries || len(r.Details) > maxMetaCacheEntries {
		return r, bad
	}
	validTime := func(at time.Time) bool { return !at.IsZero() && !at.After(now.Add(time.Minute)) }
	if r.Vaults != nil && !validTime(r.Vaults.At) {
		return r, bad
	}
	for key, e := range r.Items {
		if key == "" || !validTime(e.At) {
			return r, bad
		}
		for _, it := range e.Value {
			if it.ID == "" || it.VaultID != key {
				return r, bad
			}
		}
	}
	for key, e := range r.Details {
		if e.Value.ID == "" || e.Value.VaultID == "" || key != e.Value.VaultID+"/"+e.Value.ID || !validTime(e.At) {
			return r, bad
		}
	}
	return r, nil
}

// saveMeta serializes saves with clear/reinitialization. It takes a fresh snapshot only after winning that lock,
// so a slow old callback cannot put back data cleared by a write, a page refresh or a source change. Success is
// acknowledged before the metadata read returns. Failed writes stay dirty and are retried by a subsequent read.
func (a *app) saveMeta(ctx context.Context, gen uint64) error {
	a.metaIO.Lock()
	defer a.metaIO.Unlock()
	a.metaMu.Lock()
	if a.metaGen != gen || a.metaStore == nil || a.metaRev == a.metaSaved {
		a.metaMu.Unlock()
		return nil
	}
	r := metadataRecord{Format: metadataFormat, Version: metadataVersion, Source: a.metaSource, Seed: append([]byte(nil), a.metaSeed[:]...), Items: map[string]metadataEntry[[]itemInfo]{}, Details: map[string]metadataEntry[itemDetail]{}}
	if a.meta.vaults != nil {
		r.Vaults = &metadataEntry[[]vaultInfo]{Value: a.meta.vaults.v, At: a.meta.vaults.at}
	}
	for key, e := range a.meta.items {
		r.Items[key] = metadataEntry[[]itemInfo]{Value: e.v, At: e.at}
	}
	for key, e := range a.meta.details {
		r.Details[key] = metadataEntry[itemDetail]{Value: e.v, At: e.at}
	}
	rev, store := a.metaRev, a.metaStore
	// Marshal while the snapshot is protected; entries and their slices are otherwise read-only.
	b, err := boundedMetadata(r)
	a.metaMu.Unlock()
	if err != nil {
		return errMetadataStore
	}
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	if err := store.Save(ctx, b); err != nil {
		return errMetadataStore
	}
	a.metaMu.Lock()
	if a.metaGen == gen {
		a.metaSaved = rev
	}
	a.metaMu.Unlock()
	return nil
}

func boundedMetadata(r metadataRecord) ([]byte, error) {
	for {
		b, err := json.Marshal(r)
		if err != nil || len(b) <= maxMetadataBytes {
			return b, err
		}
		// Keep the vault list if possible; discard the oldest item/structure entries first.
		key, detail := "", false
		var at time.Time
		for k, e := range r.Items {
			if key == "" || e.At.Before(at) {
				key, at, detail = k, e.At, false
			}
		}
		for k, e := range r.Details {
			if key == "" || e.At.Before(at) {
				key, at, detail = k, e.At, true
			}
		}
		if key != "" {
			if detail {
				delete(r.Details, key)
			} else {
				delete(r.Items, key)
			}
			continue
		}
		if r.Vaults != nil {
			r.Vaults = nil
			continue
		}
		return nil, errMetadataStore
	}
}
