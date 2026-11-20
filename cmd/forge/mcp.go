package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
)

// ---------------------------------------------------------------- MCP: JSON-RPC 2.0 over Streamable HTTP

type mcpToolProperty struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpSchema(props map[string]mcpToolProperty, required ...string) map[string]any {
	properties := make(map[string]any, len(props))
	for name, prop := range props {
		properties[name] = map[string]any{"type": prop.Type, "description": prop.Description}
	}
	req := make([]any, 0, len(required))
	for _, name := range required {
		req = append(req, name)
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   req,
	}
}

// mcpTools is the fixed tool list; the array order and every field are stable between calls.
var mcpTools = []mcpTool{
	{
		Name:        "create_repo",
		Title:       "Create repository",
		Description: "Create a repository in the forge, or return the existing one with this name.",
		InputSchema: mcpSchema(map[string]mcpToolProperty{
			"name":        {"string", "The repository name."},
			"description": {"string", "A short description of the repository."},
			"private":     {"boolean", "Whether the repository is private."},
		}, "name"),
	},
	{
		Name:        "get_repo",
		Title:       "Get repository",
		Description: "Read one repository by owner and name.",
		InputSchema: mcpSchema(map[string]mcpToolProperty{
			"owner": {"string", "The account that owns the repository."},
			"repo":  {"string", "The repository name."},
		}, "owner", "repo"),
	},
	{
		Name:        "create_issue",
		Title:       "Create issue",
		Description: "Open an issue on a repository.",
		InputSchema: mcpSchema(map[string]mcpToolProperty{
			"owner": {"string", "The account that owns the repository."},
			"repo":  {"string", "The repository name."},
			"title": {"string", "The issue title."},
			"body":  {"string", "The issue body."},
		}, "owner", "repo", "title"),
	},
	{
		Name:        "list_issues",
		Title:       "List issues",
		Description: "List the issues of a repository, optionally filtered by state.",
		InputSchema: mcpSchema(map[string]mcpToolProperty{
			"owner": {"string", "The account that owns the repository."},
			"repo":  {"string", "The repository name."},
			"state": {"string", "open, closed, or all."},
		}, "owner", "repo"),
	},
	{
		Name:        "create_pull_request",
		Title:       "Create pull request",
		Description: "Open a pull request from a head branch into a base branch.",
		InputSchema: mcpSchema(map[string]mcpToolProperty{
			"owner": {"string", "The account that owns the repository."},
			"repo":  {"string", "The repository name."},
			"title": {"string", "The pull request title."},
			"body":  {"string", "The pull request body."},
			"head":  {"string", "The branch with the changes."},
			"base":  {"string", "The branch to merge into."},
		}, "owner", "repo", "title", "head", "base"),
	},
	{
		Name:        "merge_pull_request",
		Title:       "Merge pull request",
		Description: "Merge a pull request into its base branch.",
		InputSchema: mcpSchema(map[string]mcpToolProperty{
			"owner":       {"string", "The account that owns the repository."},
			"repo":        {"string", "The repository name."},
			"pull_number": {"integer", "The pull request number."},
		}, "owner", "repo", "pull_number"),
	},
}

var mcpRequired = map[string][]string{
	"create_repo":         {"name"},
	"get_repo":            {"owner", "repo"},
	"create_issue":        {"owner", "repo", "title"},
	"list_issues":         {"owner", "repo"},
	"create_pull_request": {"owner", "repo", "title", "head", "base"},
	"merge_pull_request":  {"owner", "repo", "pull_number"},
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func mcpWrite(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, body)
}

func mcpRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	mcpWrite(w, status, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func mcpRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	mcpWrite(w, 200, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if !s.authorized(r) {
		writeJSON(w, 401, errBody("Requires authentication"))
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		mcpRPCError(w, 400, nil, -32700, "Parse error")
		return
	}
	var request mcpRequest
	if err := json.Unmarshal(data, &request); err != nil {
		mcpRPCError(w, 400, nil, -32700, "Parse error")
		return
	}
	if request.Method == "" {
		mcpRPCError(w, 400, request.ID, -32600, "Invalid Request")
		return
	}
	if len(request.ID) == 0 {
		// A notification: acknowledge without a body.
		w.WriteHeader(202)
		return
	}
	switch request.Method {
	case "server/discover":
		info := map[string]any{"name": "forge", "version": "1.0.0"}
		mcpRPCResult(w, request.ID, map[string]any{
			"resultType":        "complete",
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": info,
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": info,
			},
			"instructions": "A single-process GitHub-compatible forge: repositories, issues, pull requests and git over HTTP.",
		})
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(request.Params) > 0 {
			_ = json.Unmarshal(request.Params, &params)
		}
		version := params.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		mcpRPCResult(w, request.ID, map[string]any{
			"protocolVersion": version,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{"name": "forge", "version": "1.0.0"},
		})
	case "notifications/initialized", "notifications/cancelled":
		w.WriteHeader(202)
	case "ping":
		mcpRPCResult(w, request.ID, map[string]any{})
	case "tools/list":
		mcpRPCResult(w, request.ID, map[string]any{"tools": mcpTools})
	case "tools/call":
		var params mcpCallParams
		if len(request.Params) > 0 {
			if err := json.Unmarshal(request.Params, &params); err != nil {
				mcpRPCError(w, 200, request.ID, -32602, "Invalid params")
				return
			}
		}
		result, rpcErr := s.mcpCallTool(params.Name, params.Arguments)
		if rpcErr != nil {
			mcpRPCError(w, 200, request.ID, rpcErr.code, rpcErr.message)
			return
		}
		mcpRPCResult(w, request.ID, result)
	default:
		mcpRPCError(w, 200, request.ID, -32601, "Method not found: "+request.Method)
	}
}

type mcpToolError struct {
	code    int
	message string
}

func mcpToolFailure(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	}
}

func mcpToolSuccess(value any) map[string]any {
	text, err := json.Marshal(value)
	if err != nil {
		text = []byte(`{}`)
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": value,
		"isError":           false,
	}
}

func (s *Server) mcpCallTool(name string, args map[string]any) (any, *mcpToolError) {
	known := false
	for _, tool := range mcpTools {
		if tool.Name == name {
			known = true
			break
		}
	}
	if !known {
		return nil, &mcpToolError{code: -32602, message: "Unknown tool: " + name}
	}
	for _, required := range mcpRequired[name] {
		if _, ok := args[required]; !ok {
			return mcpToolFailure("missing required argument: " + required), nil
		}
	}
	result, err := s.mcpExecute(name, args)
	if err != nil {
		return mcpToolFailure(err.Error()), nil
	}
	return mcpToolSuccess(result), nil
}

func (s *Server) mcpExecute(name string, args map[string]any) (any, error) {
	owner := stringFromAny(args["owner"])
	repoName := stringFromAny(args["repo"])
	switch name {
	case "create_repo":
		repoName = stringFromAny(args["name"])
		if repoName == "" {
			return nil, errors.New("name is required")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.viewer == "" && owner != "" {
			s.adoptLocked(owner)
		}
		repo := s.repos[repoName]
		if repo == nil {
			repo = newRepo(s.nextRepoID, repoName)
			s.nextRepoID++
			repo.Description = stringFromAny(args["description"])
			repo.Private = boolFromAny(args["private"])
			s.repos[repoName] = repo
		}
		return s.repoJSONLocked(repo), nil
	case "get_repo":
		s.mu.Lock()
		defer s.mu.Unlock()
		repo := s.repoLocked(owner, repoName)
		if repo == nil {
			return nil, errors.New("repository not found")
		}
		return s.repoJSONLocked(repo), nil
	case "create_issue":
		s.mu.Lock()
		defer s.mu.Unlock()
		repo := s.repoLocked(owner, repoName)
		if repo == nil {
			return nil, errors.New("repository not found")
		}
		is := s.createIssueLocked(repo, stringFromAny(args["title"]), stringFromAny(args["body"]), nil)
		return s.issueJSONLocked(repo, is), nil
	case "list_issues":
		s.mu.Lock()
		defer s.mu.Unlock()
		repo := s.repoLocked(owner, repoName)
		if repo == nil {
			return nil, errors.New("repository not found")
		}
		state := stringFromAny(args["state"])
		if state == "" {
			state = "all"
		}
		out := make([]ghIssue, 0, len(repo.Issues))
		for _, is := range repo.Issues {
			if state != "all" && is.State != state {
				continue
			}
			out = append(out, s.issueJSONLocked(repo, is))
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Number < out[j].Number })
		return out, nil
	case "create_pull_request":
		head := splitHeadRef(stringFromAny(args["head"]))
		base := splitHeadRef(stringFromAny(args["base"]))
		s.mu.Lock()
		defer s.mu.Unlock()
		repo := s.repoLocked(owner, repoName)
		if repo == nil {
			return nil, errors.New("repository not found")
		}
		headSha, okHead := peelRef(repo, head)
		baseSha, okBase := peelRef(repo, base)
		if !okHead || !okBase {
			return nil, errors.New("the head or base branch does not exist")
		}
		pr := s.createPullLocked(repo, stringFromAny(args["title"]), stringFromAny(args["body"]), head, headSha, base, baseSha)
		return s.pullJSONLocked(repo, pr), nil
	case "merge_pull_request":
		number := intFromAny(args["pull_number"], 0)
		s.mu.Lock()
		defer s.mu.Unlock()
		repo := s.repoLocked(owner, repoName)
		if repo == nil {
			return nil, errors.New("repository not found")
		}
		pr := repo.pullByNumberLocked(number)
		if pr == nil {
			return nil, errors.New("pull request not found")
		}
		out, status := s.mergePullLocked(repo, pr)
		if status != 200 {
			return nil, errors.New(stringFromAny(out["message"]))
		}
		return map[string]any{
			"number": number,
			"merged": true,
			"state":  "closed",
			"sha":    stringFromAny(out["sha"]),
		}, nil
	}
	return nil, errors.New("tool not implemented: " + name)
}
