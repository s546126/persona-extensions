package dingtalk

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/dingtalk/internal/dingtalk/api"
	"github.com/nwverse/persona-extensions/extensions/dingtalk/internal/dingtalk/api/apitest"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

// testManifest is a synthetic manifest of this program for the SDK's test host.
func testManifest(t *testing.T) []byte {
	t.Helper()
	cmd, _ := json.Marshal("dingtalk")
	raw, err := json.Marshal(wire.Manifest{ManifestVersion: 1, ID: ID, Name: Name, Version: "0.0.0-test",
		Engines:     &wire.Engines{ExtensionProtocol: ext.Protocol},
		Runtime:     &wire.Runtime{Command: cmd},
		Permissions: &wire.Permissions{Network: Network},
		Contributes: &wire.Contributes{Connectors: []wire.ConnectorItem{Item()}, Channel: []wire.ChannelItem{Channel()}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A file near the per-file limit must not end the extension's connection to the hub: base64 makes a 49 MB file a
// ~68.5 MB channel.receive frame, over the protocol reader's 64 MiB line (reported in review of #311). The message
// goes through the SDK's real RPC pipe, whose reader has that line, as Persona's does.
func TestLargeFileFitsTheRPCFrame(t *testing.T) {
	f := apitest.New(t)
	f.Files["large"] = make([]byte, 49<<20)
	prog := newProgram("0.0.0-test", func(o *Options) {
		o.NewClient = func(id, secret string) (*api.Client, error) {
			return api.New(api.Options{ClientID: id, ClientSecret: secret, BaseURL: f.URL(), HTTP: f.Client()})
		}
		o.Code = func() string { return code }
	})
	h := exttest.Start(t, testManifest(t), exttest.Serve(t, prog))
	var p wire.ConnectorPrompt
	if err := h.Call(ext.ConnectorNamespace+".login.step", wire.ConnectorLoginParams{Contribution: ID, Step: stepCreds,
		Values: map[string]string{"client_id": apitest.ClientID, "client_secret": apitest.ClientSecret}}, &p); err != nil || p.UserCode != code {
		t.Fatalf("binding: %+v, %v", p, err)
	}
	f.WaitConnected(t)
	f.PushMessage(t, "fp", dm("m-pair", ownerID, ownerNick, code, f))
	apitest.Eventually(t, "the pairing", func() bool { return len(f.Replies()) == 1 })

	large := dm("m-large", ownerID, ownerNick, "", f)
	large["msgtype"], large["content"] = "file", map[string]string{"downloadCode": "large", "fileName": "large.bin"}
	f.PushMessage(t, "fl", large)
	f.PushMessage(t, "fa", dm("m-after", ownerID, ownerNick, "still there?", f))
	apitest.Eventually(t, "both messages at the hub", func() bool { return len(h.Received()) == 2 })
	got := h.Received()
	if m := got[0].Message; m.ID != "m-large" || len(m.Files) != 0 || !slices.Contains(m.Unsupported, fileLabel("large.bin")+"（"+tooMuch+"）") {
		t.Fatalf("the large file: %+v", m)
	}
	if m := got[1].Message; m.ID != "m-after" || m.Text != "still there?" {
		t.Fatalf("the message after it: %+v", m)
	}
}

// The most a message may bring (four pictures and a file up to the total) still encodes under the frame line.
func TestFullBlobBudgetFitsTheFrame(t *testing.T) {
	blob := func(n int) wire.ChannelBlob {
		return wire.ChannelBlob{Name: "f", Mime: "application/octet-stream", Data: strings.Repeat("A", (n+2)/3*4)}
	}
	m := wire.ChannelMessage{ID: "m", Text: strings.Repeat("字", 64<<10)}
	for range imagesMost {
		m.Images = append(m.Images, blob(imageMost))
	}
	m.Files = []wire.ChannelBlob{blob(blobsMost - imagesMost*imageMost)}
	if n := frameSize(m); n > frameMost {
		t.Fatalf("a full message encodes to %d bytes, over %d", n, frameMost)
	}
	m.Files = append(m.Files, blob(20<<20)) // past the total: fitFrame drops it
	fitFrame(&m)
	if frameSize(m) > frameMost || len(m.Files) != 1 || len(m.Unsupported) != 1 {
		t.Fatalf("after fitting: %d bytes, %d files, %q", frameSize(m), len(m.Files), m.Unsupported)
	}
}
