// Model Context Protocol over Streamable HTTP at /mcp. Port of entry/mcp.ts.
package main

import (
	"encoding/json"
	"strings"
)

type McpRequest struct {
	Method   string
	BodyText string
	Headers  map[string]string
}

type McpResponse struct {
	Status  int
	Body    any
	Headers map[string]string
}

const mcpServerName = "makefun-github-forge"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type toolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

var mcpTools = []toolDef{
	{"create_repo", "Create a new repository owned by the authenticated user.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":        map[string]any{"type": "string", "description": "The repository name."},
			"owner":       map[string]any{"type": "string", "description": "The owner login (defaults to the authenticated user)."},
			"description": map[string]any{"type": "string", "description": "A short description of the repository."},
			"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private."},
			"full_name":   map[string]any{"type": "string", "description": "The full owner/repo name."},
		},
		"required": []any{"name"},
	}},
	{"get_repo", "Get one repository by owner and name.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"owner":     map[string]any{"type": "string", "description": "The repository owner login."},
			"repo":      map[string]any{"type": "string", "description": "The repository name."},
			"full_name": map[string]any{"type": "string", "description": "The full owner/repo name."},
		},
		"required": []any{"owner", "repo"},
	}},
	{"create_issue", "Create an issue in a repository.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"owner":     map[string]any{"type": "string", "description": "The repository owner login."},
			"repo":      map[string]any{"type": "string", "description": "The repository name."},
			"title":     map[string]any{"type": "string", "description": "The issue title."},
			"body":      map[string]any{"type": "string", "description": "The issue body."},
			"full_name": map[string]any{"type": "string", "description": "The full owner/repo name."},
		},
		"required": []any{"owner", "repo", "title"},
	}},
	{"list_issues", "List the issues of a repository.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"owner":     map[string]any{"type": "string", "description": "The repository owner login."},
			"repo":      map[string]any{"type": "string", "description": "The repository name."},
			"state":     map[string]any{"type": "string", "description": "Filter by state: open, closed or all."},
			"full_name": map[string]any{"type": "string", "description": "The full owner/repo name."},
		},
		"required": []any{"owner", "repo"},
	}},
	{"create_pull_request", "Open a pull request from a head branch into a base branch.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"owner":     map[string]any{"type": "string", "description": "The repository owner login."},
			"repo":      map[string]any{"type": "string", "description": "The repository name."},
			"title":     map[string]any{"type": "string", "description": "The pull request title."},
			"body":      map[string]any{"type": "string", "description": "The pull request body."},
			"head":      map[string]any{"type": "string", "description": "The head branch name."},
			"base":      map[string]any{"type": "string", "description": "The base branch name."},
			"full_name": map[string]any{"type": "string", "description": "The full owner/repo name."},
		},
		"required": []any{"owner", "repo", "title", "head", "base"},
	}},
	{"merge_pull_request", "Squash-merge a pull request and close it.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"owner":       map[string]any{"type": "string", "description": "The repository owner login."},
			"repo":        map[string]any{"type": "string", "description": "The repository name."},
			"pull_number": map[string]any{"type": "integer", "description": "The pull request number."},
			"number":      map[string]any{"type": "integer", "description": "Alias for pull_number."},
			"full_name":   map[string]any{"type": "string", "description": "The full owner/repo name."},
		},
		"required": []any{"owner", "repo", "pull_number"},
	}},
}

func mcpToolsJson() []any {
	var out []any
	for _, t := range mcpTools {
		out = append(out, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
	}
	return out
}

func ownerRepo(args map[string]any) (owner, repo string) {
	owner = getStr(args["owner"], getStr(args["owner_login"], getStr(args["login"])))
	// getStr with fallback only takes one fallback; do it stepwise.
	if owner == "" {
		owner = getStr(args["owner_login"])
	}
	if owner == "" {
		owner = getStr(args["login"])
	}
	repo = getStr(args["repo"])
	if repo == "" {
		repo = getStr(args["repository"])
	}
	if repo == "" {
		repo = getStr(args["repository_name"])
	}
	if repo == "" {
		repo = getStr(args["repo_name"])
	}
	fullName := getStr(args["full_name"])
	if fullName == "" {
		fullName = getStr(args["repo_full_name"])
	}
	if fullName == "" {
		fullName = getStr(args["repository_full_name"])
	}
	if (owner == "" || repo == "") && strings.Contains(fullName, "/") {
		parts := strings.SplitN(fullName, "/", 2)
		if owner == "" {
			owner = parts[0]
		}
		if repo == "" && len(parts) > 1 {
			repo = parts[1]
		}
	}
	return owner, repo
}

func toolResult(obj any) map[string]any {
	text := jsonString(obj)
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "structuredContent": obj, "isError": false}
}

func toolError(message string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": message}}, "isError": true}
}

func normalizeToolName(name string) string {
	lower := strings.ToLower(name)
	var b strings.Builder
	for _, c := range lower {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func callTool(store *Store, name string, args map[string]any) map[string]any {
	normalized := normalizeToolName(name)
	match := func(canonical string) bool {
		return normalized == canonical || strings.HasSuffix(normalized, canonical)
	}
	if match("createrepo") {
		repoName := getStr(args["name"])
		if repoName == "" {
			repoName = getStr(args["repo"])
		}
		if repoName == "" {
			repoName = getStr(args["repository"])
		}
		if repoName == "" {
			return toolError("Missing required argument: name")
		}
		description := getStr(args["description"])
		if description == "" {
			description = getStr(args["body"])
		}
		isPrivate := getBool(args["private"])
		if existing := store.RepoByName(repoName); existing != nil {
			return toolResult(store.RepoJson(existing))
		}
		created := store.CreateRepo(repoName, description, isPrivate)
		if created == nil {
			return toolError("Repository already exists")
		}
		return toolResult(store.RepoJson(created))
	}
	if match("getrepo") {
		owner, repoName := ownerRepo(args)
		if owner == "" || repoName == "" {
			return toolError("Missing required arguments: owner, repo")
		}
		repo := store.ResolveRepo(owner, repoName)
		if repo == nil {
			return toolError("Repository not found: " + owner + "/" + repoName)
		}
		return toolResult(store.RepoJson(repo))
	}
	if match("createissue") {
		owner, repoName := ownerRepo(args)
		title := getStr(args["title"])
		if title == "" {
			title = getStr(args["issue_title"])
		}
		if owner == "" || repoName == "" || title == "" {
			return toolError("Missing required arguments: owner, repo, title")
		}
		repo := store.ResolveRepo(owner, repoName)
		if repo == nil {
			return toolError("Repository not found: " + owner + "/" + repoName)
		}
		body := getStr(args["body"])
		if body == "" {
			body = getStr(args["issue_body"])
		}
		if body == "" {
			body = getStr(args["description"])
		}
		created, errMsg := store.CreateIssue(repo, title, body, nil)
		if errMsg != "" {
			return toolError(errMsg)
		}
		return toolResult(store.IssueJson(repo, created))
	}
	if match("listissues") {
		owner, repoName := ownerRepo(args)
		if owner == "" || repoName == "" {
			return toolError("Missing required arguments: owner, repo")
		}
		repo := store.ResolveRepo(owner, repoName)
		if repo == nil {
			return toolError("Repository not found: " + owner + "/" + repoName)
		}
		state := getStr(args["state"])
		if state == "" {
			state = getStr(args["status"], "open")
		}
		if state == "" {
			state = "open"
		}
		issues := store.ListIssues(repo, state)
		var list []any
		for _, issue := range issues {
			list = append(list, store.IssueJson(repo, issue))
		}
		if list == nil {
			list = []any{}
		}
		return toolResult(map[string]any{"issues": list})
	}
	if match("createpullrequest") {
		owner, repoName := ownerRepo(args)
		title := getStr(args["title"])
		if title == "" {
			title = getStr(args["pr_title"])
		}
		head := getStr(args["head"])
		if head == "" {
			head = getStr(args["head_branch"])
		}
		if head == "" {
			head = getStr(args["head_ref"])
		}
		base := getStr(args["base"])
		if base == "" {
			base = getStr(args["base_branch"])
		}
		if base == "" {
			base = getStr(args["base_ref"])
		}
		if owner == "" || repoName == "" || title == "" || head == "" || base == "" {
			return toolError("Missing required arguments: owner, repo, title, head, base")
		}
		repo := store.ResolveRepo(owner, repoName)
		if repo == nil {
			return toolError("Repository not found: " + owner + "/" + repoName)
		}
		body := getStr(args["body"])
		if body == "" {
			body = getStr(args["description"])
		}
		created, errMsg := store.CreatePull(repo, title, body, head, base)
		if errMsg != "" {
			return toolError(errMsg)
		}
		return toolResult(store.PullJson(repo, created))
	}
	if match("mergepullrequest") {
		owner, repoName := ownerRepo(args)
		raw := args["pull_number"]
		if raw == nil {
			raw = args["pull_request_number"]
		}
		if raw == nil {
			raw = args["pr_number"]
		}
		if raw == nil {
			raw = args["number"]
		}
		number, ok := getInt(raw)
		if owner == "" || repoName == "" || !ok || number < 1 {
			return toolError("Missing required arguments: owner, repo, pull_number")
		}
		repo := store.ResolveRepo(owner, repoName)
		if repo == nil {
			return toolError("Repository not found: " + owner + "/" + repoName)
		}
		pr := repo.Pulls[number]
		if pr == nil {
			return toolError("Pull request not found: " + itoa(number))
		}
		merged := store.MergePull(repo, pr)
		if !merged.OK {
			return toolError(merged.Message)
		}
		return toolResult(map[string]any{"sha": merged.Sha, "merged": true, "message": "Pull request successfully merged", "number": number})
	}
	return toolError("Unknown tool: " + name)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func handleMcp(store *Store, req *McpRequest) McpResponse {
	if req.Method == "DELETE" {
		return McpResponse{Status: 200, Body: map[string]any{}}
	}
	if req.Method != "POST" {
		return McpResponse{Status: 405, Body: map[string]any{"message": "Method Not Allowed"}}
	}
	var payload any
	if err := json.Unmarshal([]byte(req.BodyText), &payload); err != nil {
		return McpResponse{Status: 400, Body: map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error: request body is not valid JSON"}}}
	}
	rec, ok := payload.(map[string]any)
	if !ok {
		return McpResponse{Status: 400, Body: map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32600, "message": "Invalid Request: expected a JSON-RPC object"}}}
	}
	method := getStr(rec["method"])
	var id any = nil
	if v, present := rec["id"]; present {
		id = v
	}
	params := asMap(rec["params"])
	okResp := func(result any, extraHeaders ...map[string]string) McpResponse {
		var h map[string]string
		if len(extraHeaders) > 0 {
			h = extraHeaders[0]
		}
		return McpResponse{Status: 200, Body: map[string]any{"jsonrpc": "2.0", "id": id, "result": result}, Headers: h}
	}
	rpcErr := func(code int, message string) McpResponse {
		return McpResponse{Status: 200, Body: map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}}
	}
	if id == nil && strings.HasPrefix(method, "notifications/") {
		return McpResponse{Status: 202, Body: nil}
	}
	switch method {
	case "server/discover":
		return okResp(map[string]any{
			"resultType":        "discover",
			"supportedVersions": mcpSupportedVersions,
			"serverInfo":        map[string]any{"name": mcpServerName, "version": mcpServerVersion},
			"capabilities":      map[string]any{"tools": map[string]any{}, "resources": map[string]any{}, "prompts": map[string]any{}},
			"instructions":      "A GitHub-compatible git forge: repositories, issues, pull requests, git smart HTTP, GraphQL and MCP tools.",
		})
	case "initialize":
		requested := getStr(params["protocolVersion"])
		negotiated := "2025-11-25"
		for _, v := range mcpSupportedVersions {
			if v == requested {
				negotiated = v
				break
			}
		}
		return okResp(map[string]any{
			"protocolVersion": negotiated,
			"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}, map[string]string{"mcp-session-id": "mcp-session-1"})
	case "ping":
		return okResp(map[string]any{})
	case "tools/list":
		return okResp(map[string]any{"tools": mcpToolsJson()})
	case "tools/call":
		toolName := getStr(params["name"])
		args := asMap(params["arguments"])
		known := false
		for _, tool := range mcpTools {
			canonical := normalizeToolName(tool.Name)
			given := normalizeToolName(toolName)
			if given == canonical || strings.HasSuffix(given, canonical) {
				known = true
				break
			}
		}
		if !known {
			return rpcErr(-32602, "Unknown tool: "+toolName)
		}
		return okResp(callTool(store, toolName, args))
	default:
		return rpcErr(-32601, "Method not found: "+method)
	}
}
