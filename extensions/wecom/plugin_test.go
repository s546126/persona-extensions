package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/wecom/internal/wecom"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestPublishedManifestMatchesRuntime(t *testing.T) {
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != wecom.ID || m.Version != program().Version || version != m.Version {
		t.Fatal("manifest/runtime identity mismatch")
	}
	if !reflect.DeepEqual(m.Contributes.Connectors, []wire.ConnectorItem{wecom.Item()}) || !reflect.DeepEqual(m.Contributes.Channel, []wire.ChannelItem{wecom.Channel()}) {
		t.Fatal("manifest/runtime contributions differ")
	}
	if !reflect.DeepEqual(m.Permissions.Network, wecom.Network) {
		t.Fatal("manifest/runtime network permissions differ")
	}
	exttest.CheckConnectors(t, manifest, exttest.Serve(t, program()))
	exttest.CheckChannels(t, manifest, exttest.Serve(t, program()))
}
