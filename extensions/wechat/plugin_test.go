package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/wechat/internal/wechat"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestPublishedManifestMatchesRuntime(t *testing.T) {
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != wechat.ID || m.Version != program().Version || version != m.Version {
		t.Fatal("manifest/runtime identity mismatch")
	}
	if len(m.Contributes.Channel) != 1 || !m.Contributes.Channel[0].Capabilities.FilesIn || !wechat.Channel().Capabilities.FilesIn ||
		!m.Contributes.Channel[0].Capabilities.VoiceIn || !wechat.Channel().Capabilities.VoiceIn {
		t.Fatal("files_in and voice_in must be enabled in both manifest and runtime")
	}
	if !reflect.DeepEqual(m.Contributes.Connectors, []wire.ConnectorItem{wechat.Item()}) || !reflect.DeepEqual(m.Contributes.Channel, []wire.ChannelItem{wechat.Channel()}) {
		t.Fatal("manifest/runtime contributions differ")
	}
	if !reflect.DeepEqual(m.Permissions.Network, wechat.Network) {
		t.Fatal("manifest/runtime network permissions differ")
	}
	if !reflect.DeepEqual(m.Permissions.Network, []string{"*.weixin.qq.com", "*.wechat.com"}) {
		t.Fatalf("unexpected network permissions: %v", m.Permissions.Network)
	}
	exttest.CheckConnectors(t, manifest, exttest.Serve(t, program()))
	exttest.CheckChannels(t, manifest, exttest.Serve(t, program()))
}
