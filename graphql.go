// GitHub GraphQL v4: parser + executor over the same store REST uses.
// Port of entry/graphql.ts.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------

type Selection struct {
	Name         string
	Alias        string
	HasAlias     bool
	Args         map[string]any
	Selections   []*Selection
	FragmentType *string
}

type VarDef struct {
	Type       string
	Default    any
	HasDefault bool
}

type Operation struct {
	Kind       string // "query" | "mutation"
	Name       string
	Variables  map[string]VarDef
	Selections []*Selection
}

type Token struct {
	Kind  string // name | int | float | string | punct
	Value string
}

func tokenize(src string) ([]Token, error) {
	var tokens []Token
	i := 0
	for i < len(src) {
		ch := src[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if ch == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '"' {
			value := ""
			i++
			for i < len(src) && src[i] != '"' {
				if src[i] == '\\' && i+1 < len(src) {
					esc := src[i+1]
					switch esc {
					case 'n':
						value += "\n"
					case 't':
						value += "\t"
					case 'r':
						value += "\r"
					default:
						value += string(esc)
					}
					i += 2
				} else {
					value += string(src[i])
					i++
				}
			}
			i++
			tokens = append(tokens, Token{Kind: "string", Value: value})
			continue
		}
		if (ch >= '0' && ch <= '9') || (ch == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9') {
			j := i
			if src[j] == '-' {
				j++
			}
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			isFloat := false
			if j < len(src) && src[j] == '.' {
				isFloat = true
				j++
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			tokens = append(tokens, Token{Kind: kind, Value: src[i:j]})
			i = j
			continue
		}
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' {
			j := i
			for j < len(src) && ((src[j] >= 'a' && src[j] <= 'z') || (src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
				j++
			}
			tokens = append(tokens, Token{Kind: "name", Value: src[i:j]})
			i = j
			continue
		}
		if ch == '.' && i+3 <= len(src) && src[i:i+3] == "..." {
			tokens = append(tokens, Token{Kind: "punct", Value: "..."})
			i += 3
			continue
		}
		if strings.ContainsRune("!$&():=[]{}|", rune(ch)) || ch == '@' {
			tokens = append(tokens, Token{Kind: "punct", Value: string(ch)})
			i++
			continue
		}
		return nil, fmt.Errorf("unexpected character %q in GraphQL document", string(ch))
	}
	return tokens, nil
}

type gqlParser struct {
	tokens []Token
	pos    int
}

func (p *gqlParser) peek() *Token {
	if p.pos < len(p.tokens) {
		return &p.tokens[p.pos]
	}
	return nil
}

func (p *gqlParser) next() (Token, error) {
	if p.pos >= len(p.tokens) {
		return Token{}, fmt.Errorf("unexpected end of GraphQL document")
	}
	t := p.tokens[p.pos]
	p.pos++
	return t, nil
}

func (p *gqlParser) expectPunct(value string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.Kind != "punct" || t.Value != value {
		return fmt.Errorf("expected %s in GraphQL document, got %s %q", value, t.Kind, t.Value)
	}
	return nil
}

func (p *gqlParser) acceptPunct(value string) bool {
	t := p.peek()
	if t != nil && t.Kind == "punct" && t.Value == value {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) parseDocument() ([]*Operation, error) {
	var ops []*Operation
	for p.pos < len(p.tokens) {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *gqlParser) parseOperation() (*Operation, error) {
	kind := "query"
	name := ""
	first := p.peek()
	if first != nil && first.Kind == "name" && (first.Value == "query" || first.Value == "mutation") {
		kind = first.Value
		p.pos++
		if maybe := p.peek(); maybe != nil && maybe.Kind == "name" {
			name = maybe.Value
			p.pos++
		}
	}
	variables := map[string]VarDef{}
	if p.acceptPunct("(") {
		for !p.acceptPunct(")") {
			dollar, err := p.next()
			if err != nil {
				return nil, err
			}
			if dollar.Kind != "punct" || dollar.Value != "$" {
				return nil, fmt.Errorf("expected $ in variable definition")
			}
			varName, err := p.next()
			if err != nil {
				return nil, err
			}
			if varName.Kind != "name" {
				return nil, fmt.Errorf("expected a variable name")
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			typ, err := p.parseType()
			if err != nil {
				return nil, err
			}
			def := VarDef{Type: typ}
			if p.acceptPunct("=") {
				v, err := p.parseValueLiteral()
				if err != nil {
					return nil, err
				}
				def.Default = v
				def.HasDefault = true
			}
			variables[varName.Value] = def
		}
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &Operation{Kind: kind, Name: name, Variables: variables, Selections: sels}, nil
}

func (p *gqlParser) parseType() (string, error) {
	t, err := p.next()
	if err != nil {
		return "", err
	}
	var typ string
	if t.Kind == "punct" && t.Value == "[" {
		inner, err := p.parseType()
		if err != nil {
			return "", err
		}
		typ = "[" + inner + "]"
	} else if t.Kind == "name" {
		typ = t.Value
	} else {
		return "", fmt.Errorf("expected a type in GraphQL document")
	}
	if p.acceptPunct("!") {
		typ += "!"
	}
	return typ, nil
}

func (p *gqlParser) parseSelectionSet() ([]*Selection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []*Selection
	for !p.acceptPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (*Selection, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	if t.Kind == "punct" && t.Value == "..." {
		var fragmentType *string
		next := p.peek()
		if next != nil && next.Kind == "name" && next.Value == "on" {
			p.pos++
			typeName, err := p.next()
			if err != nil {
				return nil, err
			}
			if typeName.Kind != "name" {
				return nil, fmt.Errorf("expected a type name after on")
			}
			ft := typeName.Value
			fragmentType = &ft
		} else if next != nil && next.Kind == "name" {
			p.pos++
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &Selection{Name: "", Args: map[string]any{}, Selections: sels, FragmentType: fragmentType}, nil
	}
	if t.Kind != "name" {
		return nil, fmt.Errorf("expected a field name in GraphQL document")
	}
	name := t.Value
	alias := ""
	hasAlias := false
	if p.acceptPunct(":") {
		alias = name
		hasAlias = true
		real, err := p.next()
		if err != nil {
			return nil, err
		}
		if real.Kind != "name" {
			return nil, fmt.Errorf("expected a field name after an alias")
		}
		name = real.Value
	}
	args := map[string]any{}
	if p.acceptPunct("(") {
		for !p.acceptPunct(")") {
			argName, err := p.next()
			if err != nil {
				return nil, err
			}
			if argName.Kind != "name" {
				return nil, fmt.Errorf("expected an argument name")
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValueLiteral()
			if err != nil {
				return nil, err
			}
			args[argName.Value] = v
		}
	}
	for p.acceptPunct("@") {
		dir, err := p.next()
		if err != nil {
			return nil, err
		}
		if dir.Kind != "name" {
			return nil, fmt.Errorf("expected a directive name")
		}
		if p.acceptPunct("(") {
			for !p.acceptPunct(")") {
				if _, err := p.next(); err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				if _, err := p.parseValueLiteral(); err != nil {
					return nil, err
				}
			}
		}
	}
	var sels []*Selection
	if next := p.peek(); next != nil && next.Kind == "punct" && next.Value == "{" {
		sels, err = p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
	}
	return &Selection{Name: name, Alias: alias, HasAlias: hasAlias, Args: args, Selections: sels}, nil
}

func (p *gqlParser) parseValueLiteral() (any, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	if t.Kind == "punct" && t.Value == "$" {
		nameToken, err := p.next()
		if err != nil {
			return nil, err
		}
		if nameToken.Kind != "name" {
			return nil, fmt.Errorf("expected a variable name")
		}
		return map[string]any{"__variable": nameToken.Value}, nil
	}
	if t.Kind == "string" {
		return t.Value, nil
	}
	if t.Kind == "int" || t.Kind == "float" {
		f, err := strconv.ParseFloat(t.Value, 64)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	if t.Kind == "name" {
		switch t.Value {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return t.Value, nil
		}
	}
	if t.Kind == "punct" && t.Value == "[" {
		var list []any
		for !p.acceptPunct("]") {
			v, err := p.parseValueLiteral()
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		if list == nil {
			list = []any{}
		}
		return list, nil
	}
	if t.Kind == "punct" && t.Value == "{" {
		obj := map[string]any{}
		for !p.acceptPunct("}") {
			key, err := p.next()
			if err != nil {
				return nil, err
			}
			if key.Kind != "name" {
				return nil, fmt.Errorf("expected an object key")
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValueLiteral()
			if err != nil {
				return nil, err
			}
			obj[key.Value] = v
		}
		return obj, nil
	}
	return nil, fmt.Errorf("unexpected value token %s %q", t.Kind, t.Value)
}

func resolveVariables(value any, variables map[string]any) any {
	if arr, ok := value.([]any); ok {
		out := make([]any, len(arr))
		for i, v := range arr {
			out[i] = resolveVariables(v, variables)
		}
		return out
	}
	if rec, ok := value.(map[string]any); ok {
		if v, ok := rec["__variable"].(string); ok {
			if val, present := variables[v]; present {
				return val
			}
			return nil
		}
		out := map[string]any{}
		for k, v := range rec {
			out[k] = resolveVariables(v, variables)
		}
		return out
	}
	return value
}

// ---------------------------------------------------------------------------
// schema (for __type introspection)
// ---------------------------------------------------------------------------

var gqlTypeFields = map[string][]string{
	"Issue":                            {"id", "number", "title", "body", "state", "url", "createdAt", "updatedAt", "closedAt", "author", "labels", "comments", "repository"},
	"PullRequest":                      {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRef", "baseRef", "files", "url", "createdAt", "updatedAt", "mergedAt", "mergeCommitSha", "author"},
	"Repository":                       {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "url", "description", "createdAt", "updatedAt"},
	"User":                             {"id", "login", "name", "email", "url"},
	"SearchType":                       {},
	"IssueConnection":                  {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection":            {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo"},
	"PullRequestChangedFile":           {"path", "filename", "additions", "deletions", "changes", "status"},
	"PageInfo":                         {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"Ref":                              {"name", "target"},
}

var gqlEnumValues = map[string][]string{
	"SearchType":           {"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "COMMIT", "DISCUSSION", "CODE"},
	"IssueState":           {"OPEN", "CLOSED"},
	"PullRequestState":     {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":       {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"RepositoryPermission": {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"IssueOrderField":      {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":       {"ASC", "DESC"},
}

// ---------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------

type gqlCtx struct {
	Kind        string // query, repository, issue, pull, user, issues, pulls, files, comment, ref, pageInfo
	Repo        *Repo
	Issue       *Issue
	Pull        *PullRequest
	Login       string
	Issues      []*Issue
	Pulls       []*PullRequest
	Files       []PRFile
	CommentID   int
	CommentBody string
	RefName     string
	HasNextPage bool
	EndCursor   *string
	StartCursor *string
}

type gqlExecutor struct {
	store  *Store
	errors []map[string]any
}

func (e *gqlExecutor) fail(message string) any {
	e.errors = append(e.errors, map[string]any{"message": message})
	return nil
}

func selKey(sel *Selection) string {
	if sel.HasAlias {
		return sel.Alias
	}
	return sel.Name
}

func (e *gqlExecutor) execute(op *Operation, variables map[string]any) map[string]any {
	data := map[string]any{}
	for _, sel := range op.Selections {
		data[selKey(sel)] = e.resolveRoot(sel, variables)
	}
	return data
}

func (e *gqlExecutor) resolveRoot(sel *Selection, variables map[string]any) any {
	args := asMap(resolveVariables(sel.Args, variables))
	switch sel.Name {
	case "viewer":
		login := ""
		if e.store.ViewerLogin != nil {
			login = *e.store.ViewerLogin
		}
		return e.resolveFields(gqlCtx{Kind: "user", Login: login}, sel.Selections, variables, nil)
	case "repository":
		owner := getStr(args["owner"])
		name := getStr(args["name"])
		repo := e.store.ResolveRepo(owner, name)
		if repo == nil {
			return nil
		}
		return e.resolveFields(gqlCtx{Kind: "repository", Repo: repo}, sel.Selections, variables, nil)
	case "node":
		id := getStr(args["id"])
		kind, parts, ok := e.store.DecodeGlobalId(id)
		if !ok {
			return nil
		}
		switch kind {
		case "Repository":
			n, _ := strconv.Atoi(parts[0])
			repo := e.store.ReposById[n]
			if repo == nil {
				return nil
			}
			ft := "Repository"
			return e.resolveFields(gqlCtx{Kind: "repository", Repo: repo}, sel.Selections, variables, &ft)
		case "Issue":
			n, _ := strconv.Atoi(parts[0])
			num, _ := strconv.Atoi(parts[1])
			repo := e.store.ReposById[n]
			if repo == nil {
				return nil
			}
			issue := repo.Issues[num]
			if issue == nil {
				return nil
			}
			ft := "Issue"
			return e.resolveFields(gqlCtx{Kind: "issue", Repo: repo, Issue: issue}, sel.Selections, variables, &ft)
		case "PullRequest":
			n, _ := strconv.Atoi(parts[0])
			num, _ := strconv.Atoi(parts[1])
			repo := e.store.ReposById[n]
			if repo == nil {
				return nil
			}
			pull := repo.Pulls[num]
			if pull == nil {
				return nil
			}
			ft := "PullRequest"
			return e.resolveFields(gqlCtx{Kind: "pull", Repo: repo, Pull: pull}, sel.Selections, variables, &ft)
		}
		return nil
	case "__type":
		name := getStr(args["name"])
		fields, hasFields := gqlTypeFields[name]
		enumValues, hasEnum := gqlEnumValues[name]
		if !hasFields && !hasEnum {
			return nil
		}
		kind := "OBJECT"
		if hasEnum {
			kind = "ENUM"
		}
		typeObj := map[string]any{"name": name, "kind": kind}
		return e.resolveTypeFields(typeObj, fields, enumValues, sel.Selections)
	case "__schema":
		return e.resolveFields(gqlCtx{Kind: "query"}, sel.Selections, variables, nil)
	case "createIssue":
		return e.mutateCreateIssue(sel, args, variables)
	case "closeIssue":
		return e.mutateCloseIssue(sel, args, variables)
	case "createPullRequest":
		return e.mutateCreatePullRequest(sel, args, variables)
	case "mergePullRequest":
		return e.mutateMergePullRequest(sel, args, variables)
	case "addComment":
		return e.mutateAddComment(sel, args, variables)
	default:
		return e.fail("Cannot query field \"" + sel.Name + "\" on type \"Query\".")
	}
}

func (e *gqlExecutor) resolveTypeFields(typeObj map[string]any, fields, enumValues []string, selections []*Selection) map[string]any {
	out := map[string]any{}
	for _, sel := range selections {
		key := selKey(sel)
		switch sel.Name {
		case "name":
			out[key] = typeObj["name"]
		case "kind":
			out[key] = typeObj["kind"]
		case "fields":
			var arr []any
			for _, name := range fields {
				fieldObj := map[string]any{"name": name}
				for _, sub := range sel.Selections {
					if sub.Name == "name" {
						fieldObj[selKey(sub)] = name
					} else if sub.Name == "description" {
						fieldObj[selKey(sub)] = nil
					}
				}
				arr = append(arr, fieldObj)
			}
			if arr == nil {
				arr = []any{}
			}
			out[key] = arr
		case "enumValues":
			var arr []any
			for _, name := range enumValues {
				enumObj := map[string]any{"name": name}
				for _, sub := range sel.Selections {
					if sub.Name == "name" {
						enumObj[selKey(sub)] = name
					} else if sub.Name == "description" {
						enumObj[selKey(sub)] = nil
					}
				}
				arr = append(arr, enumObj)
			}
			if arr == nil {
				arr = []any{}
			}
			out[key] = arr
		default:
			e.fail("Cannot query field \"" + sel.Name + "\" on type \"__Type\".")
			out[key] = nil
		}
	}
	return out
}

func typeNameOf(v gqlCtx) string {
	switch v.Kind {
	case "repository":
		return "Repository"
	case "issue":
		return "Issue"
	case "pull":
		return "PullRequest"
	case "user":
		return "User"
	case "ref":
		return "Ref"
	case "comment":
		return "IssueComment"
	}
	return ""
}

func (e *gqlExecutor) resolveFields(value gqlCtx, selections []*Selection, variables map[string]any, fragmentType *string) map[string]any {
	out := map[string]any{}
	for _, sel := range selections {
		if sel.Name == "" && sel.FragmentType != nil {
			if fragmentType != nil && *sel.FragmentType != *fragmentType {
				continue
			}
			typeName := typeNameOf(value)
			if *sel.FragmentType != typeName && *sel.FragmentType != "Node" {
				continue
			}
			nested := e.resolveFields(value, sel.Selections, variables, nil)
			for k, v := range nested {
				out[k] = v
			}
			continue
		}
		if sel.Name == "" {
			continue
		}
		out[selKey(sel)] = e.resolveField(value, sel, variables)
	}
	return out
}

func (e *gqlExecutor) resolveField(value gqlCtx, sel *Selection, variables map[string]any) any {
	args := asMap(resolveVariables(sel.Args, variables))
	sub := sel.Selections
	hasSub := len(sub) > 0
	switch value.Kind {
	case "query":
		return e.fail("Cannot query field \"" + sel.Name + "\" on type \"Query\".")
	case "user":
		switch sel.Name {
		case "login":
			return value.Login
		case "id":
			return e.store.GlobalId("User", value.Login)
		case "name":
			return value.Login
		}
		return e.fail("Cannot query field \"" + sel.Name + "\" on type \"User\".")
	case "repository":
		repo := value.Repo
		switch sel.Name {
		case "id":
			return e.store.RepoGlobalId(repo)
		case "databaseId":
			return repo.ID
		case "name":
			return repo.Name
		case "nameWithOwner":
			return e.store.OwnerLogin(repo) + "/" + repo.Name
		case "description":
			return repo.Description
		case "url":
			return "https://example.invalid/" + e.store.OwnerLogin(repo) + "/" + repo.Name
		case "createdAt":
			return repo.CreatedAt
		case "updatedAt":
			return repo.UpdatedAt
		case "owner":
			if hasSub {
				return e.resolveFields(gqlCtx{Kind: "user", Login: e.store.OwnerLogin(repo)}, sub, variables, nil)
			}
			return e.store.OwnerLogin(repo)
		case "defaultBranchRef":
			if !hasSub {
				return map[string]any{"name": repo.DefaultBranch}
			}
			return e.resolveFields(gqlCtx{Kind: "ref", RefName: repo.DefaultBranch}, sub, variables, nil)
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "issue":
			num, _ := getInt(args["number"])
			issue := repo.Issues[num]
			if issue == nil {
				return nil
			}
			return e.resolveFields(gqlCtx{Kind: "issue", Repo: repo, Issue: issue}, sub, variables, nil)
		case "issues":
			return e.issuesConnection(repo, args, sub, variables)
		case "pullRequests":
			return e.pullsConnection(repo, args, sub, variables)
		case "pullRequest":
			num, _ := getInt(args["number"])
			pull := repo.Pulls[num]
			if pull == nil {
				return nil
			}
			return e.resolveFields(gqlCtx{Kind: "pull", Repo: repo, Pull: pull}, sub, variables, nil)
		}
		return e.fail("Cannot query field \"" + sel.Name + "\" on type \"Repository\".")
	case "issue":
		issue := value.Issue
		switch sel.Name {
		case "id":
			return e.store.IssueGlobalId(value.Repo, issue)
		case "number":
			return issue.Number
		case "title":
			return issue.Title
		case "body":
			return issue.Body
		case "state":
			return strings.ToUpper(issue.State)
		case "url":
			return fmt.Sprintf("https://example.invalid/%s/%s/issues/%d", e.store.OwnerLogin(value.Repo), value.Repo.Name, issue.Number)
		case "createdAt":
			return issue.CreatedAt
		case "updatedAt":
			return issue.UpdatedAt
		case "closedAt":
			return issue.ClosedAt
		case "repository":
			if hasSub {
				return e.resolveFields(gqlCtx{Kind: "repository", Repo: value.Repo}, sub, variables, nil)
			}
			return nil
		case "labels":
			var out []any
			for _, l := range issue.Labels {
				out = append(out, l.Name)
			}
			if out == nil {
				out = []any{}
			}
			return out
		case "comments":
			return len(issue.Comments)
		}
		return e.fail("Cannot query field \"" + sel.Name + "\" on type \"Issue\".")
	case "pull":
		pull := value.Pull
		switch sel.Name {
		case "id":
			return e.store.PullGlobalId(value.Repo, pull)
		case "number":
			return pull.Number
		case "title":
			return pull.Title
		case "body":
			return pull.Body
		case "state":
			if pull.Merged {
				return "MERGED"
			}
			return strings.ToUpper(pull.State)
		case "merged":
			return pull.Merged
		case "mergeable":
			return e.store.PRMergeable(value.Repo, pull)
		case "headRefName":
			return pull.Head
		case "baseRefName":
			return pull.Base
		case "headRef":
			if hasSub {
				return e.resolveFields(gqlCtx{Kind: "ref", RefName: pull.Head}, sub, variables, nil)
			}
			return map[string]any{"name": pull.Head}
		case "baseRef":
			if hasSub {
				return e.resolveFields(gqlCtx{Kind: "ref", RefName: pull.Base}, sub, variables, nil)
			}
			return map[string]any{"name": pull.Base}
		case "url":
			return fmt.Sprintf("https://example.invalid/%s/%s/pull/%d", e.store.OwnerLogin(value.Repo), value.Repo.Name, pull.Number)
		case "createdAt":
			return pull.CreatedAt
		case "updatedAt":
			return pull.UpdatedAt
		case "mergedAt":
			return pull.MergedAt
		case "mergeCommitSha":
			return pull.MergeCommitSha
		case "files":
			return e.filesConnection(value.Repo, pull, args, sub, variables)
		}
		return e.fail("Cannot query field \"" + sel.Name + "\" on type \"PullRequest\".")
	case "ref":
		if sel.Name == "name" {
			return value.RefName
		}
		return e.fail("Cannot query field \"" + sel.Name + "\" on type \"Ref\".")
	case "issues", "pulls", "files":
		return e.fail("Cannot query field \"" + sel.Name + "\" on a connection.")
	case "comment":
		if sel.Name == "id" {
			return e.store.GlobalId("IssueComment", value.Repo.ID, value.Issue.Number, value.CommentID)
		}
		if sel.Name == "body" {
			return value.CommentBody
		}
		return e.fail("Cannot query field \"" + sel.Name + "\" on type \"IssueComment\".")
	case "pageInfo":
		switch sel.Name {
		case "hasNextPage":
			return value.HasNextPage
		case "hasPreviousPage":
			return false
		case "startCursor":
			return value.StartCursor
		case "endCursor":
			return value.EndCursor
		}
		return e.fail("Cannot query field \"" + sel.Name + "\" on type \"PageInfo\".")
	}
	return e.fail("Cannot query field \"" + sel.Name + "\".")
}

func (e *gqlExecutor) connectionObject(nodes []gqlCtx, pageInfo gqlCtx, selections []*Selection, variables map[string]any) map[string]any {
	out := map[string]any{}
	for _, sel := range selections {
		key := selKey(sel)
		switch sel.Name {
		case "nodes":
			var arr []any
			for _, node := range nodes {
				arr = append(arr, e.resolveFields(node, sel.Selections, variables, nil))
			}
			if arr == nil {
				arr = []any{}
			}
			out[key] = arr
		case "edges":
			var arr []any
			for _, node := range nodes {
				edge := map[string]any{}
				for _, sub := range sel.Selections {
					if sub.Name == "node" {
						edge[selKey(sub)] = e.resolveFields(node, sub.Selections, variables, nil)
					} else if sub.Name == "cursor" {
						edge[selKey(sub)] = ""
					}
				}
				arr = append(arr, edge)
			}
			if arr == nil {
				arr = []any{}
			}
			out[key] = arr
		case "pageInfo":
			out[key] = e.resolveFields(pageInfo, sel.Selections, variables, nil)
		case "totalCount":
			out[key] = len(nodes)
		default:
			e.fail("Cannot query field \"" + sel.Name + "\" on a connection.")
			out[key] = nil
		}
	}
	return out
}

func (e *gqlExecutor) issuesConnection(repo *Repo, args map[string]any, selections []*Selection, variables map[string]any) any {
	list := e.store.ListIssues(repo, "all")
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := map[string]bool{}
		for _, s := range states {
			wanted[strings.ToUpper(getStr(s))] = true
		}
		var filtered []*Issue
		for _, issue := range list {
			if wanted[strings.ToUpper(issue.State)] {
				filtered = append(filtered, issue)
			}
		}
		list = filtered
	}
	if orderBy, ok := args["orderBy"].(map[string]any); ok && orderBy != nil {
		field := getStr(orderBy["field"], "CREATED_AT")
		direction := getStr(orderBy["direction"], "ASC")
		if field == "CREATED_AT" {
			// sort by number as proxy for creation order (TS sorts by number)
			if direction == "DESC" {
				for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
					list[i], list[j] = list[j], list[i]
				}
			}
		}
	} else {
		// ListIssues already returns number-ascending.
	}
	first := len(list)
	if f, ok := getInt(args["first"]); ok {
		first = f
	}
	after := -1
	if s, ok := args["after"].(string); ok {
		after = decodeCursor(s)
	}
	start := 0
	if after >= 0 {
		for i, issue := range list {
			if issue.Number == after {
				start = i + 1
				break
			}
		}
	}
	end := start + first
	if end > len(list) {
		end = len(list)
	}
	if start > len(list) {
		start = len(list)
		end = start
	}
	sliced := list[start:end]
	hasNextPage := start+first < len(list)
	var endCursor, startCursor *string
	if len(sliced) > 0 {
		ec := encodeCursor(sliced[len(sliced)-1].Number)
		sc := encodeCursor(sliced[0].Number)
		endCursor, startCursor = &ec, &sc
	}
	var nodes []gqlCtx
	for _, issue := range sliced {
		nodes = append(nodes, gqlCtx{Kind: "issue", Repo: repo, Issue: issue})
	}
	return e.connectionObject(nodes, gqlCtx{Kind: "pageInfo", HasNextPage: hasNextPage, EndCursor: endCursor, StartCursor: startCursor}, selections, variables)
}

func (e *gqlExecutor) pullsConnection(repo *Repo, args map[string]any, selections []*Selection, variables map[string]any) any {
	var list []*PullRequest
	for _, pr := range repo.Pulls {
		list = append(list, pr)
	}
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := map[string]bool{}
		for _, s := range states {
			wanted[strings.ToUpper(getStr(s))] = true
		}
		var filtered []*PullRequest
		for _, pr := range list {
			state := strings.ToUpper(pr.State)
			if pr.Merged {
				state = "MERGED"
			}
			if wanted[state] {
				filtered = append(filtered, pr)
			}
		}
		list = filtered
	} else {
		var filtered []*PullRequest
		for _, pr := range list {
			if pr.State == "open" {
				filtered = append(filtered, pr)
			}
		}
		list = filtered
	}
	// sort desc by number
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if list[j].Number > list[i].Number {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	first := len(list)
	if f, ok := getInt(args["first"]); ok {
		first = f
	}
	after := -1
	if s, ok := args["after"].(string); ok {
		after = decodeCursor(s)
	}
	start := 0
	if after >= 0 {
		for i, pr := range list {
			if pr.Number == after {
				start = i + 1
				break
			}
		}
	}
	end := start + first
	if end > len(list) {
		end = len(list)
	}
	if start > len(list) {
		start = len(list)
		end = start
	}
	sliced := list[start:end]
	hasNextPage := start+first < len(list)
	var endCursor, startCursor *string
	if len(sliced) > 0 {
		ec := encodeCursor(sliced[len(sliced)-1].Number)
		sc := encodeCursor(sliced[0].Number)
		endCursor, startCursor = &ec, &sc
	}
	var nodes []gqlCtx
	for _, pull := range sliced {
		nodes = append(nodes, gqlCtx{Kind: "pull", Repo: repo, Pull: pull})
	}
	return e.connectionObject(nodes, gqlCtx{Kind: "pageInfo", HasNextPage: hasNextPage, EndCursor: endCursor, StartCursor: startCursor}, selections, variables)
}

func (e *gqlExecutor) filesConnection(repo *Repo, pull *PullRequest, args map[string]any, selections []*Selection, variables map[string]any) any {
	files := e.store.PRFiles(repo, pull)
	first := len(files)
	if f, ok := getInt(args["first"]); ok {
		first = f
	}
	end := first
	if end > len(files) {
		end = len(files)
	}
	if end < 0 {
		end = 0
	}
	sliced := files[:end]
	out := map[string]any{}
	for _, sel := range selections {
		key := selKey(sel)
		switch sel.Name {
		case "nodes":
			var arr []any
			for _, file := range sliced {
				node := map[string]any{}
				for _, sub := range sel.Selections {
					subKey := selKey(sub)
					switch sub.Name {
					case "path", "filename":
						node[subKey] = file.Path
					case "additions":
						node[subKey] = file.Additions
					case "deletions":
						node[subKey] = file.Deletions
					case "changes":
						node[subKey] = file.Changes
					case "status":
						node[subKey] = file.Status
					default:
						e.fail("Cannot query field \"" + sub.Name + "\" on type \"PullRequestChangedFile\".")
						node[subKey] = nil
					}
				}
				arr = append(arr, node)
			}
			if arr == nil {
				arr = []any{}
			}
			out[key] = arr
		case "pageInfo":
			out[key] = e.resolveFields(gqlCtx{Kind: "pageInfo", HasNextPage: first < len(files)}, sel.Selections, variables, nil)
		case "totalCount":
			out[key] = len(files)
		default:
			e.fail("Cannot query field \"" + sel.Name + "\" on type \"PullRequestChangedFileConnection\".")
			out[key] = nil
		}
	}
	return out
}

func encodeCursor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(number)))
}

var cursorRe = regexp.MustCompile(`^cursor:(\d+)$`)

func decodeCursor(cursor string) int {
	b, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return -1
	}
	text := string(b)
	if m := cursorRe.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if n, err := strconv.Atoi(text); err == nil {
		return n
	}
	return -1
}

// ------------------------------------------------------------------ mutations

func (e *gqlExecutor) mutationPayload(sel *Selection, payload map[string]any, variables map[string]any, errorMessage string) any {
	if errorMessage != "" {
		e.fail(errorMessage)
		return nil
	}
	if payload == nil {
		return nil
	}
	out := map[string]any{}
	for _, sub := range sel.Selections {
		key := selKey(sub)
		value, present := payload[sub.Name]
		if !present {
			e.fail("Cannot query field \"" + sub.Name + "\" on a mutation payload.")
			out[key] = nil
			continue
		}
		if value != nil {
			if rec, ok := value.(map[string]any); ok {
				if ctxVal, ok := rec["___ctx"]; ok {
					if ctx, ok := ctxVal.(gqlCtx); ok {
						out[key] = e.resolveFields(ctx, sub.Selections, variables, nil)
						continue
					}
				}
				if edgeVal, ok := rec["___edge"]; ok {
					if edgeRec, ok := edgeVal.(map[string]any); ok {
						if ctxVal, ok := edgeRec["ctx"]; ok {
							if ctx, ok := ctxVal.(gqlCtx); ok {
								edgeOut := map[string]any{}
								for _, edgeSel := range sub.Selections {
									if edgeSel.Name == "node" {
										edgeOut[selKey(edgeSel)] = e.resolveFields(ctx, edgeSel.Selections, variables, nil)
									} else if edgeSel.Name == "cursor" {
										edgeOut[selKey(edgeSel)] = ""
									} else {
										e.fail("Cannot query field \"" + edgeSel.Name + "\" on an edge.")
										edgeOut[selKey(edgeSel)] = nil
									}
								}
								out[key] = edgeOut
								continue
							}
						}
					}
				}
			}
		}
		out[key] = value
	}
	return out
}

func (e *gqlExecutor) mutateCreateIssue(sel *Selection, args map[string]any, variables map[string]any) any {
	input := asMap(args["input"])
	kind, parts, ok := e.store.DecodeGlobalId(getStr(input["repositoryId"]))
	var repo *Repo
	if ok && kind == "Repository" {
		n, _ := strconv.Atoi(parts[0])
		repo = e.store.ReposById[n]
	}
	if repo == nil {
		return e.mutationPayload(sel, nil, variables, "Repository not found.")
	}
	created, errMsg := e.store.CreateIssue(repo, getStr(input["title"]), getStr(input["body"]), nil)
	if errMsg != "" {
		return e.mutationPayload(sel, nil, variables, errMsg)
	}
	return e.mutationPayload(sel, map[string]any{"issue": map[string]any{"___ctx": gqlCtx{Kind: "issue", Repo: repo, Issue: created}}}, variables, "")
}

func (e *gqlExecutor) mutateCloseIssue(sel *Selection, args map[string]any, variables map[string]any) any {
	input := asMap(args["input"])
	kind, parts, ok := e.store.DecodeGlobalId(getStr(input["issueId"]))
	var repo *Repo
	var issue *Issue
	if ok && kind == "Issue" {
		n, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		repo = e.store.ReposById[n]
		if repo != nil {
			issue = repo.Issues[num]
		}
	}
	if repo == nil || issue == nil {
		return e.mutationPayload(sel, nil, variables, "Issue not found.")
	}
	e.store.CloseIssue(repo, issue)
	return e.mutationPayload(sel, map[string]any{"issue": map[string]any{"___ctx": gqlCtx{Kind: "issue", Repo: repo, Issue: issue}}}, variables, "")
}

func (e *gqlExecutor) mutateCreatePullRequest(sel *Selection, args map[string]any, variables map[string]any) any {
	input := asMap(args["input"])
	kind, parts, ok := e.store.DecodeGlobalId(getStr(input["repositoryId"]))
	var repo *Repo
	if ok && kind == "Repository" {
		n, _ := strconv.Atoi(parts[0])
		repo = e.store.ReposById[n]
	}
	if repo == nil {
		return e.mutationPayload(sel, nil, variables, "Repository not found.")
	}
	head := getStr(input["headRefName"])
	base := getStr(input["baseRefName"])
	created, errMsg := e.store.CreatePull(repo, getStr(input["title"]), getStr(input["body"]), head, base)
	if errMsg != "" {
		for _, pr := range repo.Pulls {
			if pr.State == "open" && pr.Head == head && pr.Base == base {
				return e.mutationPayload(sel, map[string]any{"pullRequest": map[string]any{"___ctx": gqlCtx{Kind: "pull", Repo: repo, Pull: pr}}}, variables, "")
			}
		}
		return e.mutationPayload(sel, nil, variables, errMsg)
	}
	return e.mutationPayload(sel, map[string]any{"pullRequest": map[string]any{"___ctx": gqlCtx{Kind: "pull", Repo: repo, Pull: created}}}, variables, "")
}

func (e *gqlExecutor) mutateMergePullRequest(sel *Selection, args map[string]any, variables map[string]any) any {
	input := asMap(args["input"])
	kind, parts, ok := e.store.DecodeGlobalId(getStr(input["pullRequestId"]))
	var repo *Repo
	var pull *PullRequest
	if ok && kind == "PullRequest" {
		n, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		repo = e.store.ReposById[n]
		if repo != nil {
			pull = repo.Pulls[num]
		}
	}
	if repo == nil || pull == nil {
		return e.mutationPayload(sel, nil, variables, "Pull request not found.")
	}
	merged := e.store.MergePull(repo, pull)
	if !merged.OK {
		return e.mutationPayload(sel, nil, variables, merged.Message)
	}
	return e.mutationPayload(sel, map[string]any{"pullRequest": map[string]any{"___ctx": gqlCtx{Kind: "pull", Repo: repo, Pull: pull}}}, variables, "")
}

func (e *gqlExecutor) mutateAddComment(sel *Selection, args map[string]any, variables map[string]any) any {
	input := asMap(args["input"])
	kind, parts, ok := e.store.DecodeGlobalId(getStr(input["subjectId"]))
	var repo *Repo
	var issue *Issue
	if ok && kind == "Issue" {
		n, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		repo = e.store.ReposById[n]
		if repo != nil {
			issue = repo.Issues[num]
		}
	}
	if repo == nil || issue == nil {
		return e.mutationPayload(sel, nil, variables, "Issue not found.")
	}
	comment := e.store.AddComment(repo, issue, getStr(input["body"]))
	return e.mutationPayload(sel, map[string]any{
		"commentEdge": map[string]any{"___edge": map[string]any{"ctx": gqlCtx{Kind: "comment", Repo: repo, Issue: issue, CommentID: comment.ID, CommentBody: comment.Body}}},
	}, variables, "")
}

// ---------------------------------------------------------------------------
// HTTP entry point
// ---------------------------------------------------------------------------

type GraphQLResult struct {
	Status int
	Body   any
}

func handleGraphQL(store *Store, body any) GraphQLResult {
	payload := body
	if s, ok := payload.(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			return GraphQLResult{Status: 400, Body: map[string]any{"errors": []any{map[string]any{"message": "Request body is not valid JSON"}}}}
		}
		payload = parsed
	}
	runOne := func(doc any) map[string]any {
		rec, ok := doc.(map[string]any)
		if !ok {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "Each operation must be an object with a query string"}}}
		}
		query, ok := rec["query"].(string)
		if !ok || strings.TrimSpace(query) == "" {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "query is required"}}}
		}
		variables := asMap(rec["variables"])
		tokens, err := tokenize(query)
		if err != nil {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
		}
		parser := &gqlParser{tokens: tokens}
		operations, err := parser.parseDocument()
		if err != nil {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
		}
		if len(operations) == 0 {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "No operation in the document"}}}
		}
		op := operations[0]
		executor := &gqlExecutor{store: store}
		data := executor.execute(op, variables)
		result := map[string]any{"data": data}
		if len(executor.errors) > 0 {
			var errs []any
			for _, er := range executor.errors {
				errs = append(errs, er)
			}
			result["errors"] = errs
		}
		return result
	}
	if arr, ok := payload.([]any); ok {
		var out []any
		for _, doc := range arr {
			out = append(out, runOne(doc))
		}
		return GraphQLResult{Status: 200, Body: out}
	}
	return GraphQLResult{Status: 200, Body: runOne(payload)}
}
