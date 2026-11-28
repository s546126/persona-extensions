package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	op "github.com/1password/onepassword-sdk-go"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type blankAndTokenVaultOP struct {
	fakeOP
}

func (f *blankAndTokenVaultOP) listVaults(context.Context) ([]op.VaultOverview, error) {
	return []op.VaultOverview{
		{ID: "vltblank000000000000000001", Title: "", ActiveItemCount: 1},
		{ID: "vlttoken000000000000000002", Title: "Vault " + testToken[len(testToken)-12:], ActiveItemCount: 2},
	}, nil
}

func TestPageRedactsVaultTitleHandlesBlankFieldsAndClipsLongUseSubtitle(t *testing.T) {
	f := &blankAndTokenVaultOP{}
	a := newApp(func(context.Context, string) (opAPI, error) { return f, nil })
	a.token = testToken
	suffix := testToken[len(testToken)-12:]
	a.noteUse(wire.SecretsUsedEvent{
		Reference:  "op://Dev/Item/credential",
		Purpose:    wire.SecretPurpose{Kind: "extension", ID: "ext-" + suffix, Machine: "box-" + suffix},
		At:         time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
		ApprovedBy: "rule",
	})

	v := a.view(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView failed on blank vault title: %v", err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testToken) || strings.Contains(string(raw), suffix) {
		t.Fatalf("page leaked service account token fragment: %s", raw)
	}
}

func TestInitResetsCachedClientAndCacheMapsStayBounded(t *testing.T) {
	var seenTokens []string
	a := newApp(func(_ context.Context, token string) (opAPI, error) {
		seenTokens = append(seenTokens, token)
		return &fakeOP{}, nil
	})
	token1 := "ops_first_synthetic_token_00000000000000000000"
	token2 := "ops_second_synthetic_token_1111111111111111111"

	if _, err := a.init(t.Context(), &ext.Init{InitializeParams: wire.InitializeParams{
		Secrets: map[string]string{tokenSecret: token1},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.client(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.init(t.Context(), &ext.Init{InitializeParams: wire.InitializeParams{
		Secrets: map[string]string{tokenSecret: token2},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.client(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(seenTokens) != 2 || seenTokens[1] != token2 {
		t.Fatalf("init with new token reused stale client; connected tokens=%v", seenTokens)
	}

	// Metadata cache maps (items/details) must not grow unboundedly across many vaults/items.
	for i := range 200 {
		_, _ = a.readItems(t.Context(), fmt.Sprintf("vlt-%d", i), true)
	}
	a.metaMu.Lock()
	nItems := len(a.meta.items)
	a.metaMu.Unlock()
	if nItems > 128 {
		t.Fatalf("meta.items cache grew unbounded: got %d, want <= 128", nItems)
	}
}
