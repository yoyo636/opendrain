// opendrain-mcp is a minimal MCP server that exposes OpenDrain's Download
// Action Protocol to MCP-capable agents. It is dependency-free: it speaks
// JSON-RPC 2.0 on stdin/stdout and delegates every download to `opendrain dap`.
//
// The key property: the agent gets exactly one download tool ("download").
// There is no curl/wget tool in this server, so routing is unambiguous.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const (
	serverName    = "opendrain-dal"
	serverVersion = "0.3.0"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var (
	writeMu    sync.Mutex
	toolCallMu sync.Mutex // serialize tool calls so clients can rely on ordering
	dapCmd     *exec.Cmd
	dapIn      io.WriteCloser
	dapOut     io.ReadCloser
	dapMu      sync.Mutex
	dapSeq     int64
	dapPend    = map[string]chan map[string]any{}
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(serverVersion)
		return
	}
	bin := os.Getenv("OPENDRAIN_BIN")
	if bin == "" {
		// Prefer the sibling binary installed next to this server; fall back
		// to PATH so a source build still works.
		if self, err := os.Executable(); err == nil {
			sibling := filepath.Join(filepath.Dir(self), "opendrain")
			if _, err := os.Stat(sibling); err == nil {
				bin = sibling
			}
		}
	}
	if bin == "" {
		bin = "opendrain"
	}
	if p, err := exec.LookPath(bin); err == nil {
		bin = p
	}
	cmd := exec.Command(bin, "dap")
	in, err := cmd.StdinPipe()
	if err != nil {
		fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fatal(fmt.Errorf("start %s dap: %w", bin, err))
	}
	dapCmd, dapIn, dapOut = cmd, in, out
	go readDAP()

	// MCP clients expect responses in request order, and tool calls may be
	// stateful (download then verify). Handle each message synchronously.
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			sendError(nil, -32700, "parse error")
			continue
		}
		handle(req)
	}
	_ = dapIn.Close()
	_ = dapCmd.Wait()
}

func readDAP() {
	sc := bufio.NewScanner(dapOut)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var resp map[string]any
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		id, _ := resp["id"].(string)
		dapMu.Lock()
		ch := dapPend[id]
		delete(dapPend, id)
		dapMu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

func callDAP(method string, params any) (map[string]any, error) {
	dapSeq++
	id := fmt.Sprintf("mcp-%d", dapSeq)
	payload, _ := json.Marshal(map[string]any{
		"dap": "1.0", "id": id, "method": method, "params": params,
	})
	payload = append(payload, '\n')
	ch := make(chan map[string]any, 1)
	dapMu.Lock()
	dapPend[id] = ch
	_, err := dapIn.Write(payload)
	dapMu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if e, ok := r["error"].(map[string]any); ok {
			return nil, fmt.Errorf("%v: %v", e["code"], e["message"])
		}
		return r, nil
	case <-time.After(10 * time.Minute):
		return nil, fmt.Errorf("DAP request timed out")
	}
}

func handle(req rpcRequest) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		ver := p.ProtocolVersion
		if ver == "" {
			ver = "2024-11-05"
		}
		send(req.ID, map[string]any{
			"protocolVersion": ver,
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"instructions": "Use the 'download' tool for ALL external file acquisition. " +
				"Never use curl, wget, or shell download commands. The layer persists state, " +
				"resumes after interruption, verifies integrity, and returns a compact handle. " +
				"Content never enters the conversation.",
		})
	case "notifications/initialized", "initialized":
		// no response
	case "ping":
		send(req.ID, map[string]any{})
	case "tools/list":
		send(req.ID, map[string]any{"tools": tools()})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			sendError(req.ID, -32602, "invalid params")
			return
		}
		callTool(req.ID, p.Name, p.Arguments)
	case "resources/list":
		send(req.ID, map[string]any{"resources": []any{}})
	case "prompts/list":
		send(req.ID, map[string]any{"prompts": []any{}})
	default:
		sendError(req.ID, -32601, "method not found: "+req.Method)
	}
}

func tools() []map[string]any {
	return []map[string]any{
		{
			"name": "download",
			"description": "Download bytes to a local file via the OpenDrain download action layer. " +
				"This is the ONLY tool for obtaining external files; never use curl, wget, or shell downloads. " +
				"State is persisted, interrupted downloads resume automatically, integrity is verified, " +
				"and only a compact handle is returned (never file content).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":       map[string]any{"type": "string", "description": "http(s) source URL"},
					"target":    map[string]any{"type": "string", "description": "local destination path"},
					"checksum":  map[string]any{"type": "string", "description": "optional sha256:<64 hex>"},
					"task_key":  map[string]any{"type": "string", "description": "stable identity for rotating signed URLs"},
					"mirrors":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"backend":   map[string]any{"type": "string", "enum": []string{"auto", "native", "aria2"}, "default": "auto"},
					"no_resume": map[string]any{"type": "boolean", "description": "restart from scratch", "default": false},
				},
				"required": []string{"url", "target"},
			},
		},
		{
			"name": "manifest",
			"description": "List every file managed by the download layer, with size, sha256 and on-disk state. " +
				"Call this at session start to recover what previous sessions downloaded.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"verify": map[string]any{"type": "boolean", "description": "re-hash every on-disk file", "default": false},
				},
			},
		},
		{
			"name":        "verify",
			"description": "Re-hash one managed file and compare with its recorded digest. Use before trusting a cached file.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"target": map[string]any{"type": "string"}},
				"required":   []string{"target"},
			},
		},
		{
			"name": "read_excerpt",
			"description": "Read a bounded text excerpt from a local file (grep/head/tail), capped by max_bytes. " +
				"Never dumps a whole large file into context.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target":    map[string]any{"type": "string"},
					"grep":      map[string]any{"type": "string"},
					"context":   map[string]any{"type": "integer"},
					"head":      map[string]any{"type": "integer"},
					"tail":      map[string]any{"type": "integer"},
					"max_bytes": map[string]any{"type": "integer"},
				},
				"required": []string{"target"},
			},
		},
	}
}

func callTool(id json.RawMessage, name string, args map[string]any) {
	toolCallMu.Lock()
	defer toolCallMu.Unlock()
	if args == nil {
		args = map[string]any{}
	}
	var method string
	params := map[string]any{}
	switch name {
	case "download":
		method = "act"
		for _, k := range []string{"url", "target", "checksum", "task_key", "backend", "mirrors"} {
			if v, ok := args[k]; ok {
				params[k] = v
			}
		}
		if v, ok := args["no_resume"].(bool); ok {
			params["resume"] = !v
		}
	case "manifest":
		method = "manifest"
		params["verify"] = args["verify"] == true
	case "verify":
		method = "verify"
		params["target"] = args["target"]
	case "read_excerpt":
		method = "read"
		for k, v := range args {
			params[k] = v
		}
	default:
		sendError(id, -32602, "unknown tool: "+name)
		return
	}
	result, err := callDAP(method, params)
	if err != nil {
		send(id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "error: " + err.Error()}},
			"isError": true,
		})
		return
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	send(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(b)}},
	})
}

func send(id json.RawMessage, result any) {
	write(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}
func sendError(id json.RawMessage, code int, msg string) {
	write(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}
func write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	_, _ = os.Stdout.Write(append(b, '\n'))
}
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "opendrain-mcp:", err)
	os.Exit(1)
}
