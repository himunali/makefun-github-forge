// REST v3 surface under /api/v3. Port of entry/rest.ts.
package main

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type RestRequest struct {
	Method   string
	Path     string
	Query    map[string][]string
	Headers  map[string]string
	BodyText string
	Body     any
	Host     string
}

func (r *RestRequest) QueryGet(k string) string {
	if v, ok := r.Query[k]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

type RestResponse struct {
	Status  int
	Body    any
	Headers map[string]string
	BodyRaw *string
}

func restJson(status int, body any, headers ...map[string]string) RestResponse {
	var h map[string]string
	if len(headers) > 0 {
		h = headers[0]
	}
	return RestResponse{Status: status, Body: body, Headers: h}
}

func restJsonRaw(status int, bodyRaw string, headers ...map[string]string) RestResponse {
	var h map[string]string
	if len(headers) > 0 {
		h = headers[0]
	}
	return RestResponse{Status: status, Body: nil, BodyRaw: &bodyRaw, Headers: h}
}

func restErr(status int, message string) RestResponse {
	return restJson(status, map[string]any{"message": message})
}

func bodyOf(req *RestRequest) map[string]any { return asMap(req.Body) }

func paginate(req *RestRequest, items []any) (page []any, link string) {
	perPageRaw, ok := getFloat(req.QueryGet("per_page"))
	if !ok || perPageRaw == 0 {
		// TS: Number(undefined ?? '30') = 30; Number('') = 0 -> fallback 30.
		// QueryGet returns "" when absent; treat absent as 30.
		if _, present := req.Query["per_page"]; !present {
			perPageRaw = 30
		} else {
			perPageRaw = 0
		}
	}
	perPage := 30
	if !math.IsNaN(perPageRaw) && perPageRaw > 0 {
		perPage = int(math.Min(100, float64(int(perPageRaw))))
	}
	pageRaw, ok := getFloat(req.QueryGet("page"))
	if !ok {
		if _, present := req.Query["page"]; !present {
			pageRaw = 1
		} else {
			pageRaw = 0
		}
	}
	pageNum := 1
	if !math.IsNaN(pageRaw) && pageRaw > 0 {
		pageNum = int(pageRaw)
	}
	start := (pageNum - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	page = items[start:end]
	if page == nil {
		page = []any{}
	}
	if start+perPage < len(items) {
		link = fmt.Sprintf("<%s>; rel=\"next\"", fmt.Sprintf("http://%s%s?per_page=%d&page=%d", req.Host, req.Path, perPage, pageNum+1))
	}
	return page, link
}

func withLink(res RestResponse, link string) RestResponse {
	if link != "" {
		if res.Headers == nil {
			res.Headers = map[string]string{}
		}
		res.Headers["link"] = link
	}
	return res
}

func decodePathSegment(seg string) string {
	if d, err := url.PathUnescape(seg); err == nil {
		return d
	}
	return seg
}

type repoHit struct {
	repo  *Repo
	owner string
	name  string
	tail  []string
}

func repoFromSegments(store *Store, segments []string) *repoHit {
	if len(segments) < 4 || segments[0] != "api" || segments[1] != "v3" || segments[2] != "repos" {
		return nil
	}
	owner := decodePathSegment(segments[3])
	name := decodePathSegment(segments[4])
	repo := store.ResolveRepo(owner, name)
	if repo == nil {
		return nil
	}
	return &repoHit{repo: repo, owner: owner, name: name, tail: segments[5:]}
}

func commitsOf(store *Store, repo *Repo, ref string) []any {
	tip, ok := repo.Refs["refs/heads/"+ref]
	if !ok {
		tip, ok = repo.Refs[ref]
		if !ok {
			return []any{}
		}
	}
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	var out []any
	seen := map[string]bool{}
	queue := []string{tip}
	head := 0
	for head < len(queue) {
		oid := queue[head]
		head++
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		view := parseCommit(obj.Data)
		out = append(out, commitJson(store, repo, oid, view))
		queue = append(queue, view.Parents...)
	}
	if out == nil {
		out = []any{}
	}
	return out
}

func commitJson(store *Store, repo *Repo, oid string, view CommitView) map[string]any {
	owner := store.OwnerLogin(repo)
	message := view.Message
	if strings.HasSuffix(message, "\n") {
		message = message[:len(message)-1]
	}
	authorName := view.Author
	if idx := strings.Index(view.Author, " <"); idx >= 0 {
		authorName = view.Author[:idx]
	}
	committerName := view.Committer
	if idx := strings.Index(view.Committer, " <"); idx >= 0 {
		committerName = view.Committer[:idx]
	}
	authorTs := view.AuthorTs
	if authorTs == 0 {
		authorTs = store.BaseSec
	}
	committerTs := view.CommitterTs
	if committerTs == 0 {
		committerTs = store.BaseSec
	}
	var parents []any
	for _, p := range view.Parents {
		parents = append(parents, map[string]any{"sha": p})
	}
	if parents == nil {
		parents = []any{}
	}
	return map[string]any{
		"sha": oid, "node_id": store.GlobalId("Commit", oid),
		"commit": map[string]any{
			"message":      message,
			"tree":         map[string]any{"sha": view.Tree},
			"author":       map[string]any{"name": authorName, "email": "", "date": isoFromSec(authorTs)},
			"committer":    map[string]any{"name": committerName, "email": "", "date": isoFromSec(committerTs)},
			"verification": map[string]any{"verified": false, "reason": "unsigned"},
		},
		"author":    map[string]any{"login": owner, "id": 1000, "type": "User"},
		"committer": map[string]any{"login": owner, "id": 1000, "type": "User"},
		"parents":   parents,
		"url":       fmt.Sprintf("/api/v3/repos/%s/%s/commits/%s", owner, repo.Name, oid),
		"html_url":  fmt.Sprintf("https://example.invalid/%s/%s/commit/%s", owner, repo.Name, oid),
	}
}

func resolveContentsRef(repo *Repo, ref string, hasRef bool) string {
	if !hasRef || ref == "" {
		if tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			return tip
		}
		return ""
	}
	if tip, ok := repo.Refs["refs/heads/"+ref]; ok {
		return tip
	}
	if _, ok := repo.Objects[ref]; ok {
		return ref
	}
	return ""
}

func contentsAt(store *Store, repo *Repo, commitOid, path string) RestResponse {
	owner := store.OwnerLogin(repo)
	obj := repo.Objects[commitOid]
	if obj == nil || obj.Type != ObjCommit {
		return restErr(404, "Not Found")
	}
	treeOid := parseCommit(obj.Data).Tree
	base := fmt.Sprintf("/api/v3/repos/%s/%s/contents", owner, repo.Name)
	if path == "" {
		tree := repo.Objects[treeOid]
		if tree == nil {
			return restErr(404, "Not Found")
		}
		entries, err := parseTree(tree.Data)
		if err != nil {
			return restErr(404, "Not Found")
		}
		var out []any
		for _, e := range entries {
			typ := "file"
			if e.Mode == "40000" {
				typ = "dir"
			}
			size := 0
			if e.Mode != "40000" {
				if o := repo.Objects[e.OID]; o != nil {
					size = len(o.Data)
				}
			}
			out = append(out, map[string]any{
				"type": typ, "name": e.Name, "path": e.Name, "sha": e.OID, "size": size,
				"url":          base + "/" + e.Name,
				"html_url":     fmt.Sprintf("https://example.invalid/%s/%s/blob/%s/%s", owner, repo.Name, repo.DefaultBranch, e.Name),
				"git_url":      fmt.Sprintf("/api/v3/repos/%s/%s/git/blobs/%s", owner, repo.Name, e.OID),
				"download_url": nil,
			})
		}
		if out == nil {
			out = []any{}
		}
		return restJson(200, out)
	}
	segments := splitNonEmpty(path, "/")
	currentTree := treeOid
	for i, seg := range segments {
		treeObj := repo.Objects[currentTree]
		if treeObj == nil || treeObj.Type != ObjTree {
			return restErr(404, "Not Found")
		}
		entries, err := parseTree(treeObj.Data)
		if err != nil {
			return restErr(404, "Not Found")
		}
		var entry *TreeEntry
		for j := range entries {
			if entries[j].Name == seg {
				entry = &entries[j]
				break
			}
		}
		if entry == nil {
			return restErr(404, "Not Found")
		}
		last := i == len(segments)-1
		if !last {
			if entry.Mode != "40000" {
				return restErr(404, "Not Found")
			}
			currentTree = entry.OID
			continue
		}
		if entry.Mode == "40000" {
			dirObj := repo.Objects[entry.OID]
			if dirObj == nil || dirObj.Type != ObjTree {
				return restErr(404, "Not Found")
			}
			children, err := parseTree(dirObj.Data)
			if err != nil {
				return restErr(404, "Not Found")
			}
			var out []any
			for _, child := range children {
				typ := "file"
				if child.Mode == "40000" {
					typ = "dir"
				}
				size := 0
				if child.Mode != "40000" {
					if o := repo.Objects[child.OID]; o != nil {
						size = len(o.Data)
					}
				}
				out = append(out, map[string]any{
					"type": typ, "name": child.Name, "path": path + "/" + child.Name, "sha": child.OID, "size": size,
					"url":          base + "/" + path + "/" + child.Name,
					"html_url":     fmt.Sprintf("https://example.invalid/%s/%s/blob/%s/%s/%s", owner, repo.Name, repo.DefaultBranch, path, child.Name),
					"git_url":      fmt.Sprintf("/api/v3/repos/%s/%s/git/blobs/%s", owner, repo.Name, child.OID),
					"download_url": nil,
				})
			}
			if out == nil {
				out = []any{}
			}
			return restJson(200, out)
		}
		blob := repo.Objects[entry.OID]
		if blob == nil {
			return restErr(404, "Not Found")
		}
		return restJson(200, map[string]any{
			"type": "file", "encoding": "base64", "size": len(blob.Data),
			"name": segments[len(segments)-1], "path": path,
			"content": b64Encode(blob.Data), "sha": entry.OID,
			"url":          base + "/" + path,
			"html_url":     fmt.Sprintf("https://example.invalid/%s/%s/blob/%s/%s", owner, repo.Name, repo.DefaultBranch, path),
			"git_url":      fmt.Sprintf("/api/v3/repos/%s/%s/git/blobs/%s", owner, repo.Name, entry.OID),
			"download_url": nil,
		})
	}
	return restErr(404, "Not Found")
}

func labelNamesOf(value any) []string {
	arr, ok := value.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		} else if m, ok := e.(map[string]any); ok {
			if name, ok := m["name"].(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

func stripBranchPrefix(ref string) string {
	if strings.Contains(ref, ":") {
		parts := strings.Split(ref, ":")
		return parts[len(parts)-1]
	}
	return ref
}

// ---------------------------------------------------------------------------
// router
// ---------------------------------------------------------------------------

func handleRest(store *Store, req *RestRequest) *RestResponse {
	segments := splitNonEmpty(req.Path, "/")
	if len(segments) < 2 || segments[0] != "api" || segments[1] != "v3" {
		return nil
	}
	method := req.Method

	if (req.Path == "/api/v3" || req.Path == "/api/v3/") && method == "GET" {
		r := restJson(200, map[string]any{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"}, map[string]string{"x-oauth-scopes": "repo, read:org"})
		return &r
	}
	if req.Path == "/api/v3/meta" && method == "GET" {
		r := restJson(200, map[string]any{"verifiable_password_authentication": false, "installed_version": "1.0.0", "github_services_sha": "0000000000000000000000000000000000000000"})
		return &r
	}
	if req.Path == "/api/v3/user" && method == "GET" {
		if store.ViewerLogin == nil {
			r := restErr(404, "Not Found")
			return &r
		}
		login := *store.ViewerLogin
		r := restJson(200, map[string]any{
			"login": login, "id": 1000, "node_id": store.GlobalId("User", login),
			"type": "User", "name": login, "email": login + "@example.invalid",
			"html_url": "https://example.invalid/" + login, "url": "/api/v3/user",
		})
		return &r
	}
	if req.Path == "/api/v3/user/repos" && method == "GET" {
		var items []any
		for _, repo := range store.ListRepos() {
			items = append(items, store.RepoJson(repo))
		}
		page, link := paginate(req, items)
		r := withLink(restJson(200, page), link)
		return &r
	}
	if req.Path == "/api/v3/user/repos" && method == "POST" {
		body := bodyOf(req)
		name := getStr(body["name"])
		if name == "" {
			r := restErr(422, "name is required")
			return &r
		}
		created := store.CreateRepo(name, getStr(body["description"]), getBool(body["private"]))
		if created == nil {
			r := restErr(422, "Repository already exists")
			return &r
		}
		r := restJson(201, store.RepoJson(created))
		return &r
	}

	// /repos/{owner}/{name}[...]
	if len(segments) >= 4 && segments[0] == "api" && segments[1] == "v3" && segments[2] == "repos" {
		hit := repoFromSegments(store, segments)
		if hit == nil {
			r := restErr(404, "Not Found")
			return &r
		}
		repo, owner := hit.repo, hit.owner
		tail := hit.tail

		if len(tail) == 0 {
			if method == "GET" {
				r := restJson(200, store.RepoJson(repo))
				return &r
			}
			if method == "PATCH" {
				body := bodyOf(req)
				if d, ok := body["description"].(string); ok {
					repo.Description = d
				}
				if p, ok := body["private"].(bool); ok {
					repo.IsPrivate = p
				}
				if db, ok := body["default_branch"].(string); ok {
					if _, exists := repo.Refs["refs/heads/"+db]; exists {
						repo.DefaultBranch = db
					}
				}
				r := restJson(200, store.RepoJson(repo))
				return &r
			}
			if method == "DELETE" {
				store.DeleteRepo(repo)
				r := RestResponse{Status: 204, Body: nil}
				return &r
			}
			r := restErr(404, "Not Found")
			return &r
		}

		// Branches.
		if len(tail) == 1 && tail[0] == "branches" && method == "GET" {
			var out []any
			for _, branch := range store.BranchNames(repo) {
				out = append(out, map[string]any{
					"name":      branch,
					"commit":    map[string]any{"sha": repo.Refs["refs/heads/"+branch], "url": ""},
					"protected": false,
				})
			}
			if out == nil {
				out = []any{}
			}
			r := restJson(200, out)
			return &r
		}
		if len(tail) == 2 && tail[0] == "branches" && method == "GET" {
			branch := decodePathSegment(tail[1])
			tip, ok := repo.Refs["refs/heads/"+branch]
			if !ok {
				r := restErr(404, "Not Found")
				return &r
			}
			r := restJson(200, map[string]any{"name": branch, "commit": map[string]any{"sha": tip, "url": ""}, "protected": false})
			return &r
		}

		// Commits.
		if len(tail) == 1 && tail[0] == "commits" && method == "GET" {
			ref := req.QueryGet("sha")
			if _, present := req.Query["sha"]; !present {
				ref = repo.DefaultBranch
			}
			r := restJson(200, commitsOf(store, repo, ref))
			return &r
		}

		// Git Data: refs.
		if len(tail) >= 2 && tail[0] == "git" && tail[1] == "refs" {
			if len(tail) == 2 && method == "POST" {
				body := bodyOf(req)
				ref := getStr(body["ref"])
				sha := getStr(body["sha"])
				if !strings.HasPrefix(ref, "refs/") || sha == "" {
					r := restErr(422, "ref and sha are required")
					return &r
				}
				if _, exists := repo.Refs[ref]; exists {
					r := restErr(422, "Reference already exists")
					return &r
				}
				if _, exists := repo.Objects[sha]; !exists {
					r := restErr(422, "Object does not exist")
					return &r
				}
				repo.Refs[ref] = sha
				store.bumpGen(repo)
				obj := repo.Objects[sha]
				typ := "commit"
				if obj != nil {
					typ = string(obj.Type)
				}
				r := restJson(201, map[string]any{"ref": ref, "node_id": store.GlobalId("Ref", ref), "url": "", "object": map[string]any{"sha": sha, "type": typ, "url": ""}})
				return &r
			}
			if len(tail) == 2 && method == "GET" {
				var refs []any
				// TS uses Map insertion order; sort by ref name for determinism.
				var names []string
				for ref := range repo.Refs {
					names = append(names, ref)
				}
				sort.Strings(names)
				for _, ref := range names {
					sha := repo.Refs[ref]
					typ := "commit"
					if o := repo.Objects[sha]; o != nil {
						typ = string(o.Type)
					}
					refs = append(refs, map[string]any{"ref": ref, "node_id": store.GlobalId("Ref", ref), "url": "", "object": map[string]any{"sha": sha, "type": typ, "url": ""}})
				}
				if refs == nil {
					refs = []any{}
				}
				r := restJson(200, refs)
				return &r
			}
			if len(tail) >= 3 && method == "GET" {
				kind := tail[2]
				parts := make([]string, 0, len(tail)-3)
				for _, p := range tail[3:] {
					parts = append(parts, decodePathSegment(p))
				}
				branch := strings.Join(parts, "/")
				ref := "refs/" + kind + "/" + branch
				tip, ok := repo.Refs[ref]
				if !ok {
					r := restErr(404, "Not Found")
					return &r
				}
				typ := "commit"
				if o := repo.Objects[tip]; o != nil {
					typ = string(o.Type)
				}
				r := restJson(200, map[string]any{"ref": ref, "node_id": store.GlobalId("Ref", ref), "url": "", "object": map[string]any{"sha": tip, "type": typ, "url": ""}})
				return &r
			}
			r := restErr(404, "Not Found")
			return &r
		}

		// Contents.
		if len(tail) >= 1 && tail[0] == "contents" {
			parts := make([]string, 0, len(tail)-1)
			for _, p := range tail[1:] {
				parts = append(parts, decodePathSegment(p))
			}
			contentPath := strings.Join(parts, "/")
			if method == "GET" {
				ref, hasRef := req.Query["ref"]
				refVal := ""
				if hasRef && len(ref) > 0 {
					refVal = ref[0]
				}
				commitOid := resolveContentsRef(repo, refVal, hasRef)
				if commitOid == "" {
					r := restErr(404, "Not Found")
					return &r
				}
				r := contentsAt(store, repo, commitOid, contentPath)
				return &r
			}
			if method == "PUT" {
				body := bodyOf(req)
				contentB64 := getStr(body["content"])
				if contentB64 == "" {
					r := restErr(422, "content is required")
					return &r
				}
				content, err := b64Decode(contentB64)
				if err != nil {
					r := restErr(422, "content is not valid base64")
					return &r
				}
				shaVal, hasSha := body["sha"].(string)
				result := store.CommitFile(repo, contentPath, content, getStr(body["message"], "Update "+contentPath), getStr(body["branch"], repo.DefaultBranch), shaVal, hasSha)
				if !result.OK {
					r := restErr(result.Status, result.Message)
					return &r
				}
				status := 200
				if result.Created {
					status = 201
				}
				r := restJson(status, map[string]any{
					"content": map[string]any{
						"name": lastSegment(contentPath), "path": contentPath, "sha": result.BlobSha,
						"size": len(content), "encoding": "base64", "content": contentB64,
						"url": "", "html_url": "", "git_url": "", "download_url": nil,
					},
					"commit": map[string]any{"sha": result.CommitSha, "message": getStr(body["message"])},
				})
				return &r
			}
			if method == "DELETE" {
				body := bodyOf(req)
				shaVal, hasSha := body["sha"].(string)
				result := store.DeleteFile(repo, contentPath, getStr(body["message"], "Delete "+contentPath), getStr(body["branch"], repo.DefaultBranch), shaVal, hasSha)
				if !result.OK {
					r := restErr(result.Status, result.Message)
					return &r
				}
				r := restJson(200, map[string]any{"commit": map[string]any{"sha": result.CommitSha}})
				return &r
			}
			r := restErr(404, "Not Found")
			return &r
		}

		// Labels.
		if len(tail) == 1 && tail[0] == "labels" {
			if method == "GET" {
				var labels []*Label
				for _, l := range repo.Labels {
					labels = append(labels, l)
				}
				sort.Slice(labels, func(i, j int) bool { return labels[i].ID < labels[j].ID })
				var out []any
				for _, l := range labels {
					out = append(out, store.LabelJson(l))
				}
				if out == nil {
					out = []any{}
				}
				r := restJson(200, out)
				return &r
			}
			if method == "POST" {
				body := bodyOf(req)
				labelName := getStr(body["name"])
				if labelName == "" {
					r := restErr(422, "name is required")
					return &r
				}
				label := store.CreateLabel(repo, labelName, getStr(body["color"], "ededed"), getStr(body["description"]))
				r := restJson(201, store.LabelJson(label))
				return &r
			}
			r := restErr(404, "Not Found")
			return &r
		}

		// Issues.
		if len(tail) >= 1 && tail[0] == "issues" {
			if r := handleIssues(store, req, repo, tail); r != nil {
				return r
			}
			r := restErr(404, "Not Found")
			return &r
		}

		// Pull requests.
		if len(tail) >= 1 && tail[0] == "pulls" {
			if r := handlePulls(store, req, repo, owner, tail); r != nil {
				return r
			}
			r := restErr(404, "Not Found")
			return &r
		}

		r := restErr(404, "Not Found")
		return &r
	}

	r := restErr(404, "Not Found")
	return &r
}

func lastSegment(p string) string {
	parts := strings.Split(p, "/")
	return parts[len(parts)-1]
}

func handleIssues(store *Store, req *RestRequest, repo *Repo, tail []string) *RestResponse {
	method := req.Method
	if len(tail) == 1 {
		if method == "GET" {
			state := req.QueryGet("state")
			if _, present := req.Query["state"]; !present {
				state = "open"
			}
			issues := store.ListIssues(repo, state)
			sortBy := req.QueryGet("sort")
			if _, present := req.Query["sort"]; !present {
				sortBy = "created"
			}
			direction := req.QueryGet("direction")
			if _, present := req.Query["direction"]; !present {
				direction = "desc"
			}
			if sortBy == "created" {
				sort.SliceStable(issues, func(i, j int) bool {
					if direction == "asc" {
						return issues[i].CreatedSeq < issues[j].CreatedSeq
					}
					return issues[i].CreatedSeq > issues[j].CreatedSeq
				})
			}
			var items []any
			for _, issue := range issues {
				items = append(items, store.IssueJson(repo, issue))
			}
			page, link := paginate(req, items)
			r := withLink(restJson(200, page), link)
			return &r
		}
		if method == "POST" {
			body := bodyOf(req)
			title := getStr(body["title"])
			if title == "" {
				r := restErr(422, "title is required")
				return &r
			}
			created, errMsg := store.CreateIssue(repo, title, getStr(body["body"]), labelNamesOf(body["labels"]))
			if errMsg != "" {
				r := restErr(422, errMsg)
				return &r
			}
			r := restJsonRaw(201, store.IssueJsonString(repo, created))
			return &r
		}
		return nil
	}
	number, err := strconv.Atoi(tail[1])
	if err != nil || number < 1 {
		r := restErr(404, "Not Found")
		return &r
	}
	issue := repo.Issues[number]
	if len(tail) == 2 {
		if issue == nil {
			r := restErr(404, "Not Found")
			return &r
		}
		if method == "GET" {
			r := restJsonRaw(200, store.IssueJsonString(repo, issue))
			return &r
		}
		if method == "PATCH" {
			body := bodyOf(req)
			if t, ok := body["title"].(string); ok {
				issue.Title = t
			}
			if b, ok := body["body"].(string); ok {
				issue.Body = b
			}
			if getStr(body["state"]) == "closed" {
				store.CloseIssue(repo, issue)
			} else if getStr(body["state"]) == "open" {
				store.ReopenIssue(repo, issue)
			}
			if arr, ok := body["labels"].([]any); ok {
				names := labelNamesOf(arr)
				var labels []*Label
				for _, labelName := range names {
					label, ok := repo.Labels[labelName]
					if !ok {
						r := restErr(422, "Label does not exist: "+labelName)
						return &r
					}
					labels = append(labels, label)
				}
				issue.Labels = labels
			}
			r := restJsonRaw(200, store.IssueJsonString(repo, issue))
			return &r
		}
		return nil
	}
	if len(tail) == 3 && tail[2] == "comments" {
		if issue == nil {
			r := restErr(404, "Not Found")
			return &r
		}
		if method == "GET" {
			var out []any
			for _, c := range issue.Comments {
				out = append(out, store.CommentJson(repo, issue, c))
			}
			if out == nil {
				out = []any{}
			}
			r := restJson(200, out)
			return &r
		}
		if method == "POST" {
			body := bodyOf(req)
			commentBody := getStr(body["body"])
			if commentBody == "" {
				r := restErr(422, "body is required")
				return &r
			}
			comment := store.AddComment(repo, issue, commentBody)
			r := restJson(201, store.CommentJson(repo, issue, comment))
			return &r
		}
		return nil
	}
	return nil
}

func handlePulls(store *Store, req *RestRequest, repo *Repo, owner string, tail []string) *RestResponse {
	method := req.Method
	if len(tail) == 1 {
		if method == "GET" {
			state := req.QueryGet("state")
			if _, present := req.Query["state"]; !present {
				state = "open"
			}
			var pulls []*PullRequest
			for _, pr := range repo.Pulls {
				pulls = append(pulls, pr)
			}
			if state != "all" {
				var filtered []*PullRequest
				for _, pr := range pulls {
					if pr.State == state {
						filtered = append(filtered, pr)
					}
				}
				pulls = filtered
			}
			sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
			var items []any
			for _, pr := range pulls {
				items = append(items, store.PullJson(repo, pr))
			}
			page, link := paginate(req, items)
			r := withLink(restJson(200, page), link)
			return &r
		}
		if method == "POST" {
			body := bodyOf(req)
			title := getStr(body["title"])
			head := stripBranchPrefix(getStr(body["head"]))
			base := stripBranchPrefix(getStr(body["base"]))
			if title == "" || head == "" || base == "" {
				r := restErr(422, "title, head and base are required")
				return &r
			}
			created, errMsg := store.CreatePull(repo, title, getStr(body["body"]), head, base)
			if errMsg != "" {
				r := restErr(422, errMsg)
				return &r
			}
			r := restJson(201, store.PullJson(repo, created))
			return &r
		}
		return nil
	}
	number, err := strconv.Atoi(tail[1])
	if err != nil || number < 1 {
		r := restErr(404, "Not Found")
		return &r
	}
	pr := repo.Pulls[number]
	if len(tail) == 2 {
		if pr == nil {
			r := restErr(404, "Not Found")
			return &r
		}
		if method == "GET" {
			r := restJson(200, store.PullJson(repo, pr))
			return &r
		}
		return nil
	}
	if len(tail) == 3 && tail[2] == "files" && method == "GET" {
		if pr == nil {
			r := restErr(404, "Not Found")
			return &r
		}
		var files []any
		for _, f := range store.PRFiles(repo, pr) {
			files = append(files, map[string]any{
				"sha": "", "filename": f.Path, "status": f.Status,
				"additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes,
				"blob_url": "", "raw_url": "", "contents_url": "", "patch": "",
			})
		}
		if files == nil {
			files = []any{}
		}
		r := restJson(200, files)
		return &r
	}
	if len(tail) == 3 && tail[2] == "merge" && method == "PUT" {
		if pr == nil {
			r := restErr(404, "Not Found")
			return &r
		}
		merged := store.MergePull(repo, pr)
		if !merged.OK {
			r := restErr(merged.Status, merged.Message)
			return &r
		}
		r := restJson(200, map[string]any{"sha": merged.Sha, "merged": true, "message": "Pull request successfully merged"})
		return &r
	}
	return nil
}
