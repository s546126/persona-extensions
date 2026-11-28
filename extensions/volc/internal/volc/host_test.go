package volc

import (
	"encoding/json"
	"net/url"
	"os"
	"slices"
	"testing"
)

// The service the program calls is the one host the manifest asks the network for: the hub's sandbox allows nothing
// else.
func TestServiceHostIsPermitted(t *testing.T) {
	raw, err := os.ReadFile("../../persona-extension.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Permissions struct {
			Network []string `json:"network"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{asrBase, ttsURL, consoleURL} {
		u, err := url.Parse(addr)
		if err != nil {
			t.Fatal(err)
		}
		if addr == consoleURL { // opened by the owner's browser, not by the program
			continue
		}
		if !slices.Contains(m.Permissions.Network, u.Hostname()) || u.Scheme != "wss" && u.Scheme != "https" {
			t.Errorf("%s is not on the manifest's network %v", addr, m.Permissions.Network)
		}
	}
}
