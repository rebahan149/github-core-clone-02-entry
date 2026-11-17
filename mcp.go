package main

// MCP (Model Context Protocol) over JSON-RPC 2.0 at POST /mcp. Implements the
// modern discovery era (server/discover) plus a legacy initialize fallback.
// The six tools act on the same store as REST and GraphQL.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
)

var mcpTools = []map[string]any{
	{
		"name":        "create_repo",
		"description": "Create a repository owned by the authenticated user.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "Repository name."},
				"description": map[string]any{"type": "string", "description": "Repository description."},
				"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private."},
			},
			"required": []string{"name"},
		},
	},
	{
		"name":        "get_repo",
		"description": "Get a repository by owner and name.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":  map[string]any{"type": "string", "description": "Repository name."},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		"name":        "create_issue",
		"description": "Create an issue in a repository.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":  map[string]any{"type": "string", "description": "Repository name."},
				"title": map[string]any{"type": "string", "description": "Issue title."},
				"body":  map[string]any{"type": "string", "description": "Issue body."},
			},
			"required": []string{"owner", "repo", "title"},
		},
	},
	{
		"name":        "list_issues",
		"description": "List issues in a repository.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":  map[string]any{"type": "string", "description": "Repository name."},
				"state": map[string]any{"type": "string", "description": "Issue state filter: open, closed or all."},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		"name":        "create_pull_request",
		"description": "Create a pull request from a head branch into a base branch.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":  map[string]any{"type": "string", "description": "Repository name."},
				"title": map[string]any{"type": "string", "description": "Pull request title."},
				"body":  map[string]any{"type": "string", "description": "Pull request body."},
				"head":  map[string]any{"type": "string", "description": "Head branch name."},
				"base":  map[string]any{"type": "string", "description": "Base branch name."},
			},
			"required": []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		"name":        "merge_pull_request",
		"description": "Merge a pull request (squash) by number.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":       map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":        map[string]any{"type": "string", "description": "Repository name."},
				"pull_number": map[string]any{"type": "integer", "description": "Pull request number."},
			},
			"required": []string{"owner", "repo", "pull_number"},
		},
	},
}

func mcpToolByName(name string) map[string]any {
	for _, t := range mcpTools {
		if t["name"] == name {
			return t
		}
	}
	return nil
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcEnvelope struct {
	key string
	val any
	id  json.RawMessage
}

func rpcResult(id json.RawMessage, result any) rpcEnvelope {
	return rpcEnvelope{key: "result", val: result, id: id}
}

func rpcError(id json.RawMessage, code int, message string) rpcEnvelope {
	return rpcEnvelope{key: "error", val: map[string]any{"code": code, "message": message}, id: id}
}

// rpcResponse marshals a JSON-RPC envelope with the request's id spliced in
// byte-identically.
func rpcResponse(env rpcEnvelope) []byte {
	valBytes, _ := json.Marshal(env.val)
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":"2.0","id":`)
	if env.id == nil {
		buf.WriteString("null")
	} else {
		buf.Write(env.id)
	}
	buf.WriteString(`,"` + env.key + `":`)
	buf.Write(valBytes)
	buf.WriteString("}")
	return buf.Bytes()
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	if !authed(r) {
		writeErr(w, 401, "Bad credentials")
		return
	}
	body, err := readRawBody(r)
	if err != nil {
		writeJSONBytes(w, 400, rpcResponse(rpcError(nil, -32700, "Parse error")))
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		writeJSONBytes(w, 400, rpcResponse(rpcError(nil, -32700, "Parse error")))
		return
	}
	if req.ID == nil {
		// notification: no response
		w.WriteHeader(202)
		return
	}
	switch req.Method {
	case "server/discover":
		writeJSONBytes(w, 200, rpcResponse(rpcResult(req.ID, map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "forge", "version": "1.0.0"},
		})))
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &params)
		pv := params.ProtocolVersion
		if pv == "" {
			pv = "2025-11-25"
		}
		writeJSONBytes(w, 200, rpcResponse(rpcResult(req.ID, map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
		})))
	case "ping":
		writeJSONBytes(w, 200, rpcResponse(rpcResult(req.ID, map[string]any{})))
	case "tools/list":
		writeJSONBytes(w, 200, rpcResponse(rpcResult(req.ID, map[string]any{"tools": mcpTools})))
	case "tools/call":
		handleMCPCall(w, req)
	default:
		writeJSONBytes(w, 200, rpcResponse(rpcError(req.ID, -32601, "Method not found")))
	}
}

func handleMCPCall(w http.ResponseWriter, req rpcRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSONBytes(w, 200, rpcResponse(rpcError(req.ID, -32602, "Invalid params")))
		return
	}
	tool := mcpToolByName(params.Name)
	if tool == nil {
		writeJSONBytes(w, 200, rpcResponse(rpcError(req.ID, -32602, "Unknown tool: "+params.Name)))
		return
	}
	args := params.Arguments
	if args == nil {
		args = map[string]any{}
	}
	// required-argument validation against the declared schema
	schema := tool["inputSchema"].(map[string]any)
	for _, reqName := range schema["required"].([]string) {
		if _, ok := args[reqName]; !ok {
			writeJSONBytes(w, 200, rpcResponse(rpcError(req.ID, -32602, "Missing required argument: "+reqName)))
			return
		}
	}
	result, isErr := mcpExecute(params.Name, args)
	text, _ := json.Marshal(result)
	content := []any{map[string]any{"type": "text", "text": string(text)}}
	res := map[string]any{"content": content, "isError": isErr}
	if !isErr {
		res["structuredContent"] = result
	}
	writeJSONBytes(w, 200, rpcResponse(rpcResult(req.ID, res)))
}

// mcpExecute runs a tool; the returned value is the structured result.
func mcpExecute(name string, args map[string]any) (any, bool) {
	argS := func(k string) string {
		s, _ := args[k].(string)
		return s
	}
	switch name {
	case "create_repo":
		repoName := argS("name")
		desc := argS("description")
		priv, _ := args["private"].(bool)
		repo, created := store.createRepo(viewerLogin, repoName, desc, priv)
		if !created {
			repo = store.getRepo(viewerLogin, repoName)
		}
		if repo == nil {
			return map[string]any{"message": "could not create repository"}, true
		}
		var out map[string]any
		withRepoRead(repo, func() { out = repoJSON(repo) })
		return out, false
	case "get_repo":
		repo := store.getRepo(argS("owner"), argS("repo"))
		if repo == nil {
			return map[string]any{"message": "repository not found"}, true
		}
		var out map[string]any
		withRepoRead(repo, func() { out = repoJSON(repo) })
		return out, false
	case "create_issue":
		repo := store.getRepo(argS("owner"), argS("repo"))
		if repo == nil {
			return map[string]any{"message": "repository not found"}, true
		}
		var out map[string]any
		withRepoWrite(repo, func() {
			iss := createIssueIn(repo, argS("title"), argS("body"), nil)
			if iss != nil {
				out = issueJSON(repo, iss)
			}
		})
		if out == nil {
			return map[string]any{"message": "could not create issue"}, true
		}
		return out, false
	case "list_issues":
		repo := store.getRepo(argS("owner"), argS("repo"))
		if repo == nil {
			return map[string]any{"message": "repository not found"}, true
		}
		state := argS("state")
		if state == "" {
			state = "open"
		}
		out := []any{}
		withRepoRead(repo, func() {
			var list []*Issue
			for _, iss := range repo.Issues {
				if state == "all" || iss.State == state {
					list = append(list, iss)
				}
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
			for _, iss := range list {
				out = append(out, issueJSON(repo, iss))
			}
		})
		return out, false
	case "create_pull_request":
		repo := store.getRepo(argS("owner"), argS("repo"))
		if repo == nil {
			return map[string]any{"message": "repository not found"}, true
		}
		var out map[string]any
		var fail bool
		withRepoWrite(repo, func() {
			head, base := argS("head"), argS("base")
			if _, ok := repo.Refs["refs/heads/"+head]; !ok {
				fail = true
				return
			}
			if _, ok := repo.Refs["refs/heads/"+base]; !ok {
				fail = true
				return
			}
			p := createPullIn(repo, argS("title"), argS("body"), head, base)
			out = pullJSON(repo, p)
		})
		if fail || out == nil {
			return map[string]any{"message": "could not create pull request"}, true
		}
		return out, false
	case "merge_pull_request":
		repo := store.getRepo(argS("owner"), argS("repo"))
		if repo == nil {
			return map[string]any{"message": "repository not found"}, true
		}
		numF, ok := args["pull_number"].(float64)
		if !ok {
			if n, ok2 := args["pull_number"].(int); ok2 {
				numF, ok = float64(n), true
			}
		}
		if !ok {
			return map[string]any{"message": "pull_number is required"}, true
		}
		num := int(numF)
		var out map[string]any
		var fail bool
		withRepoWrite(repo, func() {
			p := repo.Pulls[num]
			if p == nil {
				fail = true
				return
			}
			if !p.Merged {
				_, conflicts := store.mergePullSquash(repo, p)
				if len(conflicts) > 0 {
					fail = true
					return
				}
			}
			out = pullJSON(repo, p)
		})
		if fail || out == nil {
			return map[string]any{"message": "could not merge pull request"}, true
		}
		return out, false
	}
	return map[string]any{"message": "unknown tool"}, true
}
