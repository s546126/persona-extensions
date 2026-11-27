package main

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Relingo must work without a terminal: it starts no other program and
// contributes nothing that needs a command-line tool on the user's computer.
func TestRelingoNeedsNoCommandLine(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if imp.Path.Value == `"os/exec"` {
				t.Errorf("%s imports os/exec: Relingo must not start other programs", name)
			}
		}
	}
	raw, err := os.ReadFile("persona-extension.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Contributes map[string]json.RawMessage `json:"contributes"`
		Tools       json.RawMessage            `json:"tools"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Tools) > 0 {
		t.Error("manifest installs command-line tools")
	}
	for point := range m.Contributes {
		switch point {
		case "ui", "text_lookup":
		default:
			t.Errorf("unexpected contribution %q: keep Relingo free of command-line dependencies", point)
		}
	}
}
