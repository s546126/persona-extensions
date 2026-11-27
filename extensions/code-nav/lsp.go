package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// A minimal Language Server Protocol client: one language server process per repository root, spoken to over its
// stdin and stdout (JSON-RPC 2.0 with Content-Length framing). It asks only what the page needs: definitions,
// implementations, references, hover, a file's symbols and a search over the workspace's symbols. It never sends an edit.

// lspPos is a position as LSP writes it: a 0-based line and a 0-based character in the negotiated encoding.
type lspPos struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPos `json:"start"`
	End   lspPos `json:"end"`
}

type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

// lspLocationOrLink reads both Location and LocationLink (a server may answer either).
type lspLocationOrLink struct {
	URI                  string   `json:"uri"`
	Range                lspRange `json:"range"`
	TargetURI            string   `json:"targetUri"`
	TargetSelectionRange lspRange `json:"targetSelectionRange"`
}

func (l lspLocationOrLink) location() lspLocation {
	if l.TargetURI != "" {
		return lspLocation{URI: l.TargetURI, Range: l.TargetSelectionRange}
	}
	return lspLocation{URI: l.URI, Range: l.Range}
}

// lspSymbol reads both DocumentSymbol (hierarchical) and SymbolInformation (flat, with a location).
type lspSymbol struct {
	Name           string       `json:"name"`
	Detail         string       `json:"detail"`
	Kind           int          `json:"kind"`
	Range          lspRange     `json:"range"`
	SelectionRange lspRange     `json:"selectionRange"`
	Children       []lspSymbol  `json:"children"`
	Location       *lspLocation `json:"location"`
	ContainerName  string       `json:"containerName"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("language server: %s (%d)", e.Message, e.Code) }

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// lspServer is one running language server for one root.
type lspServer struct {
	root     string
	cmd      *exec.Cmd
	in       io.WriteCloser
	encoding string // "utf-8" or "utf-16", as the server chose

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcMessage
	opened  map[string]openDoc // the files told to the server
	order   []string           // opened uris, oldest first
	last    time.Time
	dead    chan struct{}
	err     error
}

// openDoc is a file the server was told about: the version it has and a digest of the text it got.
type openDoc struct {
	version int
	sum     [32]byte
}

// maxOpen bounds the files a server is told about at once; the oldest are closed first.
const maxOpen = 64

// startLSP starts bin in root and runs initialize. env is the program's environment (PATH, HOME, GOPATH...).
// readOnlySettings are the settings that keep servers from writing into a repository or running its code: no
// build scripts, proc macros or check on save for rust-analyzer (they run cargo, build.rs and write target/), no
// background index for sourcekit-lsp (it builds into .build/), no type acquisition for typescript-language-server.
// They go with initialize and answer the server's workspace/configuration under their section.
var readOnlySettings = map[string]any{
	"rust-analyzer": map[string]any{
		"cargo":        map[string]any{"buildScripts": map[string]any{"enable": false}, "autoreload": false},
		"procMacro":    map[string]any{"enable": false},
		"checkOnSave":  false,
		"cachePriming": map[string]any{"enable": false},
	},
	"sourcekit-lsp":              map[string]any{"backgroundIndexing": false},
	"typescript-language-server": map[string]any{"disableAutomaticTypingAcquisition": true},
}

// initOptions are a server's initializationOptions, by its program's name.
func initOptions(bin string) any {
	name := filepath.Base(bin)
	if name == "sourcekit-lsp" {
		return map[string]any{"backgroundIndexing": false}
	}
	return readOnlySettings[name]
}

func startLSP(ctx context.Context, bin string, args []string, root string, env []string) (*lspServer, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = root
	cmd.Env = env
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &lspServer{root: root, cmd: cmd, in: in, pending: map[int]chan rpcMessage{}, opened: map[string]openDoc{}, last: time.Now(), dead: make(chan struct{})}
	go s.read(bufio.NewReaderSize(out, 1<<16))
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		if s.err == nil {
			s.err = fmt.Errorf("language server exited: %v", err)
		}
		s.mu.Unlock()
	}()

	uri := fileURI(root)
	init := map[string]any{
		"processId":             os.Getpid(),
		"rootUri":               uri,
		"workspaceFolders":      []map[string]string{{"uri": uri, "name": filepath.Base(root)}},
		"clientInfo":            map[string]string{"name": "persona-code-nav"},
		"initializationOptions": initOptions(bin),
		"capabilities": map[string]any{
			"general": map[string]any{"positionEncodings": []string{"utf-8", "utf-16"}},
			"textDocument": map[string]any{
				"definition":     map[string]any{"linkSupport": true},
				"implementation": map[string]any{"linkSupport": true},
				"hover":          map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
				"references":     map[string]any{},
				"documentSymbol": map[string]any{"hierarchicalDocumentSymbolSupport": true},
				"synchronization": map[string]any{
					"didSave": false,
				},
			},
			"workspace": map[string]any{"symbol": map[string]any{}, "workspaceFolders": true, "configuration": true},
			"window":    map[string]any{"workDoneProgress": false},
		},
	}
	var res struct {
		Capabilities struct {
			PositionEncoding string `json:"positionEncoding"`
		} `json:"capabilities"`
	}
	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.call(ictx, "initialize", init, &res); err != nil {
		s.kill()
		return nil, err
	}
	s.encoding = "utf-16"
	if res.Capabilities.PositionEncoding == "utf-8" {
		s.encoding = "utf-8"
	}
	if err := s.notify("initialized", map[string]any{}); err != nil {
		s.kill()
		return nil, err
	}
	return s, nil
}

func (s *lspServer) alive() bool {
	select {
	case <-s.dead:
		return false
	default:
		return true
	}
}

// read takes every message the server sends: answers go to their callers, requests get the answer a read-only client
// gives (nothing to configure, no progress shown), notifications are dropped.
func (s *lspServer) read(r *bufio.Reader) {
	defer func() {
		s.mu.Lock()
		for id, ch := range s.pending {
			close(ch)
			delete(s.pending, id)
		}
		s.mu.Unlock()
		close(s.dead)
	}()
	for {
		msg, err := readMessage(r)
		if err != nil {
			s.mu.Lock()
			if s.err == nil {
				s.err = fmt.Errorf("language server: %v", err)
			}
			s.mu.Unlock()
			return
		}
		switch {
		case msg.Method != "" && len(msg.ID) > 0: // a request from the server
			var result any
			if msg.Method == "workspace/configuration" {
				var p struct {
					Items []struct {
						Section string `json:"section"`
					} `json:"items"`
				}
				_ = json.Unmarshal(msg.Params, &p)
				answers := make([]any, len(p.Items))
				for i, it := range p.Items {
					answers[i] = readOnlySettings[it.Section]
				}
				result = answers
			}
			_ = s.write(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		case msg.Method != "": // a notification
		default:
			id, err := strconv.Atoi(string(msg.ID))
			if err != nil {
				continue
			}
			s.mu.Lock()
			ch := s.pending[id]
			delete(s.pending, id)
			s.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		}
	}
}

// readMessage reads one framed message: headers up to an empty line, then Content-Length bytes.
func readMessage(r *bufio.Reader) (rpcMessage, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return rpcMessage{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 || n > 64<<20 {
				return rpcMessage{}, fmt.Errorf("bad Content-Length %q", v)
			}
			length = n
		}
	}
	if length < 0 {
		return rpcMessage{}, errors.New("message without Content-Length")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return rpcMessage{}, err
	}
	var msg rpcMessage
	if err := json.Unmarshal(buf, &msg); err != nil {
		return rpcMessage{}, err
	}
	return msg, nil
}

func (s *lspServer) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := fmt.Fprintf(s.in, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
		return err
	}
	_, err = s.in.Write(b)
	return err
}

func (s *lspServer) notify(method string, params any) error {
	return s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// call sends a request and waits for its answer, or for ctx.
func (s *lspServer) call(ctx context.Context, method string, params, out any) error {
	s.mu.Lock()
	if s.err != nil && !s.alive() {
		err := s.err
		s.mu.Unlock()
		return err
	}
	s.nextID++
	id := s.nextID
	ch := make(chan rpcMessage, 1)
	s.pending[id] = ch
	s.last = time.Now()
	s.mu.Unlock()
	if err := s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		s.forget(id)
		return err
	}
	select {
	case msg, ok := <-ch:
		if !ok {
			s.mu.Lock()
			err := s.err
			s.mu.Unlock()
			if err == nil {
				err = errors.New("language server stopped")
			}
			return err
		}
		if msg.Error != nil {
			return msg.Error
		}
		if out == nil || len(msg.Result) == 0 || string(msg.Result) == "null" {
			return nil
		}
		return json.Unmarshal(msg.Result, out)
	case <-ctx.Done():
		s.forget(id)
		_ = s.notify("$/cancelRequest", map[string]any{"id": id})
		return ctx.Err()
	}
}

func (s *lspServer) forget(id int) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// open tells the server about a file before asking about it (once per file, until it is pushed out by newer ones).
func (s *lspServer) open(path, languageID, text string) error {
	uri := fileURI(path)
	sum := sha256.Sum256([]byte(text))
	s.mu.Lock()
	if o, ok := s.opened[uri]; ok {
		if o.sum == sum {
			s.mu.Unlock()
			return nil
		}
		// the file changed on disk since the server was told: give it the text as it is now
		o.version++
		o.sum = sum
		s.opened[uri] = o
		s.mu.Unlock()
		return s.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": o.version},
			"contentChanges": []map[string]string{{"text": text}},
		})
	}
	s.opened[uri] = openDoc{version: 1, sum: sum}
	s.order = append(s.order, uri)
	var drop []string
	for len(s.order) > maxOpen {
		drop = append(drop, s.order[0])
		delete(s.opened, s.order[0])
		s.order = s.order[1:]
	}
	s.mu.Unlock()
	for _, u := range drop {
		_ = s.notify("textDocument/didClose", map[string]any{"textDocument": map[string]string{"uri": u}})
	}
	return s.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": uri, "languageId": languageID, "version": 1, "text": text,
	}})
}

func (s *lspServer) idleSince() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// stop asks the server to shut down and exit, and kills it when it does not in a few seconds.
func (s *lspServer) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if s.alive() {
		_ = s.call(ctx, "shutdown", nil, nil)
		_ = s.notify("exit", nil)
	}
	select {
	case <-s.dead:
	case <-ctx.Done():
	}
	s.kill()
}

func (s *lspServer) kill() {
	_ = s.in.Close()
	if s.cmd.Process != nil {
		killProcessGroup(s.cmd)
	}
}

// fileURI is a path as a file:// URI.
func fileURI(path string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	return u.String()
}

// uriPath is a file:// URI's path, or "" for any other kind.
func uriPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return filepath.FromSlash(u.Path)
}

// toLSP turns a column counted in characters (1-based) on line into the server's encoding (0-based).
func toLSP(line string, col int, encoding string) int {
	n, units := 0, 0
	for _, r := range line {
		if n >= col-1 {
			break
		}
		n++
		if encoding == "utf-8" {
			units += utf8.RuneLen(r)
		} else {
			units += len(utf16.Encode([]rune{r}))
		}
	}
	return units
}

// fromLSP turns the server's character on line (0-based, its encoding) into a column counted in characters (1-based).
func fromLSP(line string, character int, encoding string) int {
	col, units := 1, 0
	for _, r := range line {
		if units >= character {
			break
		}
		col++
		if encoding == "utf-8" {
			units += utf8.RuneLen(r)
		} else {
			units += len(utf16.Encode([]rune{r}))
		}
	}
	return col
}
