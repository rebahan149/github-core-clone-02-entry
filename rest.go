package main

// REST v3 surface: root, meta, user, repositories, contents (backed by the git
// object store), git data (refs/branches/commits) and labels. Issues and pull
// requests live in issues.go.

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func handleREST(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		writeErr(w, 401, "Bad credentials")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v3")
	segs := strings.FieldsFunc(rest, func(c rune) bool { return c == '/' })

	if len(segs) == 0 {
		if r.Method == http.MethodGet {
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			writeJSON(w, 200, map[string]any{
				"current_user_url": "/api/v3/user",
				"repository_url":   "/api/v3/repos/{owner}/{repo}",
			})
			return
		}
		writeErr(w, 404, "Not Found")
		return
	}

	switch segs[0] {
	case "meta":
		if r.Method == http.MethodGet && len(segs) == 1 {
			writeJSON(w, 200, map[string]any{"installed_version": "1.0.0-forge", "verifiable_password_authentication": false})
			return
		}
	case "user":
		if len(segs) == 1 && r.Method == http.MethodGet {
			writeJSON(w, 200, viewerJSON())
			return
		}
		if len(segs) == 2 && segs[1] == "repos" {
			switch r.Method {
			case http.MethodGet:
				restListUserRepos(w, r)
				return
			case http.MethodPost:
				restCreateRepo(w, r)
				return
			}
		}
	case "repos":
		if len(segs) >= 3 {
			owner, name := segs[1], segs[2]
			repo := store.getRepo(owner, name)
			if repo == nil {
				writeErr(w, 404, "Not Found")
				return
			}
			restRepoDispatch(w, r, repo, segs[3:])
			return
		}
	}
	writeErr(w, 404, "Not Found")
}

func viewerJSON() map[string]any {
	return map[string]any{
		"login": viewerLogin,
		"id":    viewerID,
		"type":  "User",
		"name":  viewerName,
		"email": viewerEmail,
	}
}

func repoJSON(rp *Repo) map[string]any {
	return map[string]any{
		"id":             rp.ID,
		"name":           rp.Name,
		"full_name":      rp.Owner + "/" + rp.Name,
		"owner":          map[string]any{"login": rp.Owner, "id": viewerID, "type": "User"},
		"private":        rp.Private,
		"description":    rp.Description,
		"default_branch": rp.DefaultBranch,
	}
}

func restCreateRepo(w http.ResponseWriter, r *http.Request) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	name, has := strField(body, "name")
	if !has || strings.TrimSpace(name) == "" {
		writeErr(w, 422, "Repository creation failed: name is required")
		return
	}
	desc, _ := strField(body, "description")
	priv, _ := boolField(body, "private")
	repo, created := store.createRepo(viewerLogin, name, desc, priv)
	if !created {
		writeErr(w, 422, "Repository creation failed: name already exists on this account")
		return
	}
	writeJSON(w, 201, repoJSON(repo))
}

func pageParams(r *http.Request) (perPage, page int) {
	perPage = 30
	page = 1
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	return
}

func restListUserRepos(w http.ResponseWriter, r *http.Request) {
	repos := store.listRepos(viewerLogin)
	perPage, page := pageParams(r)
	start := (page - 1) * perPage
	if start > len(repos) {
		start = len(repos)
	}
	end := start + perPage
	if end > len(repos) {
		end = len(repos)
	}
	out := []any{}
	for _, rp := range repos[start:end] {
		out = append(out, repoJSON(rp))
	}
	if end < len(repos) {
		q := r.URL.Query()
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page+1))
		next := fmt.Sprintf("http://%s%s?%s", r.Host, r.URL.Path, q.Encode())
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
	}
	writeJSON(w, 200, out)
}

func restRepoDispatch(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			var out map[string]any
			withRepoRead(repo, func() { out = repoJSON(repo) })
			writeJSON(w, 200, out)
		case http.MethodPatch:
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			withRepoWrite(repo, func() {
				if v, has := strField(body, "description"); has {
					repo.Description = v
				}
				if v, has := boolField(body, "private"); has {
					repo.Private = v
				}
				if v, has := strField(body, "default_branch"); has && v != "" {
					repo.DefaultBranch = v
				}
			})
			if v, has := strField(body, "name"); has && v != "" && v != repo.Name {
				store.mu.Lock()
				delete(store.repos, repo.key())
				repo.Name = v
				store.repos[repo.key()] = repo
				store.mu.Unlock()
			}
			var out map[string]any
			withRepoRead(repo, func() { out = repoJSON(repo) })
			writeJSON(w, 200, out)
		case http.MethodDelete:
			store.deleteRepo(repo.Owner, repo.Name)
			w.WriteHeader(204)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}

	switch rest[0] {
	case "contents":
		restContents(w, r, repo, strings.Join(rest[1:], "/"))
	case "git":
		restGitData(w, r, repo, rest[1:])
	case "branches":
		restBranches(w, r, repo, rest[1:])
	case "commits":
		if r.Method == http.MethodGet && len(rest) == 1 {
			restListCommits(w, r, repo)
			return
		}
		writeErr(w, 404, "Not Found")
	case "labels":
		restLabels(w, r, repo, rest[1:])
	case "issues":
		restIssues(w, r, repo, rest[1:])
	case "pulls":
		restPulls(w, r, repo, rest[1:])
	default:
		writeErr(w, 404, "Not Found")
	}
}

// ---- contents ----

// resolveRefToCommit resolves a ref name (branch, tag or raw oid) to a commit.
func resolveRefToCommit(repo *Repo, ref string) (oid, bool) {
	if ref == "" {
		ref = repo.DefaultBranch
	}
	if tip, ok := repo.getRef("refs/heads/" + ref); ok {
		return tip, true
	}
	if tip, ok := repo.getRef("refs/tags/" + ref); ok {
		c, ok := store.peelToCommit(tip)
		return c, ok
	}
	if o, err := oidFromHex(ref); err == nil {
		if obj := store.getObject(o); obj != nil && obj.Type == "commit" {
			return o, true
		}
		if c, ok := store.peelToCommit(o); ok {
			return c, true
		}
	}
	return zeroOID, false
}

func commitTreeOf(commit oid) (oid, bool) {
	obj := store.getObject(commit)
	if obj == nil || obj.Type != "commit" {
		return zeroOID, false
	}
	ci, err := parseCommit(obj.Data)
	if err != nil {
		return zeroOID, false
	}
	return ci.Tree, true
}

func restContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	switch r.Method {
	case http.MethodGet:
		commit, ok := resolveRefToCommit(repo, r.URL.Query().Get("ref"))
		if !ok {
			writeErr(w, 404, "Not Found")
			return
		}
		tree, ok := commitTreeOf(commit)
		if !ok {
			writeErr(w, 404, "Not Found")
			return
		}
		entry, entries, kind := store.treeAtPath(tree, path)
		switch kind {
		case "file":
			blob := store.getObject(entry.Oid)
			if blob == nil {
				writeErr(w, 404, "Not Found")
				return
			}
			writeJSON(w, 200, map[string]any{
				"type":     "file",
				"name":     entry.Name,
				"path":     path,
				"sha":      entry.Oid.Hex(),
				"size":     len(blob.Data),
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString(blob.Data),
			})
		case "dir":
			out := []any{}
			for _, e := range entries {
				typ := "file"
				if e.IsDir() {
					typ = "dir"
				}
				childPath := e.Name
				if path != "" {
					childPath = path + "/" + e.Name
				}
				out = append(out, map[string]any{"type": typ, "name": e.Name, "path": childPath, "sha": e.Oid.Hex()})
			}
			writeJSON(w, 200, out)
		default:
			writeErr(w, 404, "Not Found")
		}
	case http.MethodPut:
		restPutContent(w, r, repo, path)
	case http.MethodDelete:
		restDeleteContent(w, r, repo, path)
	default:
		writeErr(w, 404, "Not Found")
	}
}

// branchTipForWrite resolves the branch a contents write targets. It reports
// (tip, exists). A missing default branch on an unborn repo is creatable.
func branchTipForWrite(repo *Repo, branch string) (oid, bool, bool) {
	if tip, ok := repo.Refs["refs/heads/"+branch]; ok {
		return tip, true, true
	}
	if branch == repo.DefaultBranch && len(repo.Refs) == 0 {
		return zeroOID, false, true
	}
	return zeroOID, false, false
}

func restPutContent(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	contentB64, has := strField(body, "content")
	if !has {
		writeErr(w, 422, "content is required")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		writeErr(w, 422, "content is not valid base64")
		return
	}
	message, _ := strField(body, "message")
	branch, hasBranch := strField(body, "branch")
	if !hasBranch || branch == "" {
		branch = repo.DefaultBranch
	}
	shaGiven, hasSha := strField(body, "sha")

	var status int
	var commitSHA string
	var blobSHA string
	var failStatus int
	var failMsg string
	withRepoWrite(repo, func() {
		tip, tipExists, ok := branchTipForWrite(repo, branch)
		if !ok {
			failStatus, failMsg = 422, "branch does not exist"
			return
		}
		var flat map[string]treeEntry
		if tipExists {
			tree, tok := commitTreeOf(tip)
			if !tok {
				failStatus, failMsg = 422, "branch tip is not a commit"
				return
			}
			flat = store.flattenTree(tree)
		} else {
			flat = map[string]treeEntry{}
		}
		existing, exists := flat[path]
		if exists {
			if !hasSha {
				failStatus, failMsg = 422, "sha wasn't supplied"
				return
			}
			if !strings.EqualFold(shaGiven, existing.Oid.Hex()) {
				failStatus, failMsg = 409, "sha does not match the current content"
				return
			}
			status = 200
		} else {
			status = 201
		}
		blob := store.putObject("blob", raw)
		flat[path] = treeEntry{Mode: "100644", Name: baseName(path), Oid: blob}
		newTree := store.buildTreeFromFlat(flat)
		ts := store.nowUnix()
		ident := viewerIdent(ts)
		var parents []oid
		if tipExists {
			parents = []oid{tip}
		}
		commitData := buildCommit(newTree, parents, ident, ident, message+"\n")
		commit := store.putObject("commit", commitData)
		repo.Refs["refs/heads/"+branch] = commit
		commitSHA = commit.Hex()
		blobSHA = blob.Hex()
	})
	if failStatus != 0 {
		writeErr(w, failStatus, failMsg)
		return
	}
	writeJSON(w, status, map[string]any{
		"content": map[string]any{
			"type":     "file",
			"name":     baseName(path),
			"path":     path,
			"sha":      blobSHA,
			"encoding": "base64",
		},
		"commit": map[string]any{"sha": commitSHA},
	})
}

func restDeleteContent(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	message, _ := strField(body, "message")
	branch, hasBranch := strField(body, "branch")
	if !hasBranch || branch == "" {
		branch = repo.DefaultBranch
	}
	shaGiven, hasSha := strField(body, "sha")
	var commitSHA string
	var failStatus int
	var failMsg string
	withRepoWrite(repo, func() {
		tip, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			failStatus, failMsg = 404, "Not Found"
			return
		}
		tree, tok := commitTreeOf(tip)
		if !tok {
			failStatus, failMsg = 404, "Not Found"
			return
		}
		flat := store.flattenTree(tree)
		existing, exists := flat[path]
		if !exists {
			failStatus, failMsg = 404, "Not Found"
			return
		}
		if hasSha && !strings.EqualFold(shaGiven, existing.Oid.Hex()) {
			failStatus, failMsg = 409, "sha does not match the current content"
			return
		}
		delete(flat, path)
		newTree := store.buildTreeFromFlat(flat)
		ts := store.nowUnix()
		ident := viewerIdent(ts)
		commitData := buildCommit(newTree, []oid{tip}, ident, ident, message+"\n")
		commit := store.putObject("commit", commitData)
		repo.Refs["refs/heads/"+branch] = commit
		commitSHA = commit.Hex()
	})
	if failStatus != 0 {
		writeErr(w, failStatus, failMsg)
		return
	}
	writeJSON(w, 200, map[string]any{
		"content": nil,
		"commit":  map[string]any{"sha": commitSHA},
	})
}

func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// ---- git data ----

func restGitData(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) >= 1 && rest[0] == "refs" {
		if len(rest) == 1 && r.Method == http.MethodPost {
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			ref, _ := strField(body, "ref")
			shaHex, _ := strField(body, "sha")
			target, err := oidFromHex(shaHex)
			if !strings.HasPrefix(ref, "refs/") || err != nil {
				writeErr(w, 422, "Validation Failed")
				return
			}
			obj := store.getObject(target)
			if obj == nil || obj.Type != "commit" {
				writeErr(w, 422, "Validation Failed: sha is not a commit")
				return
			}
			created := false
			withRepoWrite(repo, func() {
				if _, exists := repo.Refs[ref]; !exists {
					repo.Refs[ref] = target
					created = true
				}
			})
			if !created {
				writeErr(w, 422, "Reference already exists")
				return
			}
			writeJSON(w, 201, map[string]any{
				"ref":    ref,
				"object": map[string]any{"sha": target.Hex(), "type": "commit"},
			})
			return
		}
		if len(rest) >= 3 && rest[1] == "heads" && r.Method == http.MethodGet {
			ref := "refs/heads/" + strings.Join(rest[2:], "/")
			tip, ok := repo.getRef(ref)
			if !ok {
				writeErr(w, 404, "Not Found")
				return
			}
			writeJSON(w, 200, map[string]any{
				"ref":    ref,
				"object": map[string]any{"sha": tip.Hex(), "type": "commit"},
			})
			return
		}
	}
	writeErr(w, 404, "Not Found")
}

func restBranches(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if r.Method != http.MethodGet {
		writeErr(w, 404, "Not Found")
		return
	}
	if len(rest) == 0 {
		snap := repo.refSnapshot()
		out := []any{}
		for _, name := range repo.sortedRefNames() {
			if !strings.HasPrefix(name, "refs/heads/") {
				continue
			}
			short := strings.TrimPrefix(name, "refs/heads/")
			out = append(out, map[string]any{
				"name":      short,
				"commit":    map[string]any{"sha": snap[name].Hex()},
				"protected": false,
			})
		}
		writeJSON(w, 200, out)
		return
	}
	branch := strings.Join(rest, "/")
	tip, ok := repo.getRef("refs/heads/" + branch)
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, map[string]any{
		"name":      branch,
		"commit":    map[string]any{"sha": tip.Hex()},
		"protected": false,
	})
}

func commitJSON(commit oid) map[string]any {
	obj := store.getObject(commit)
	ci, _ := parseCommit(obj.Data)
	aName, aEmail, aTs := identParts(ci.Author)
	cName, cEmail, cTs := identParts(ci.Commit)
	parents := []any{}
	for _, p := range ci.Parents {
		parents = append(parents, map[string]any{"sha": p.Hex()})
	}
	return map[string]any{
		"sha": commit.Hex(),
		"commit": map[string]any{
			"author":    map[string]any{"name": aName, "email": aEmail, "date": rfc3339(aTs)},
			"committer": map[string]any{"name": cName, "email": cEmail, "date": rfc3339(cTs)},
			"message":   strings.TrimRight(ci.Message, "\n"),
			"tree":      map[string]any{"sha": ci.Tree.Hex()},
		},
		"parents": parents,
	}
}

func restListCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	ref := r.URL.Query().Get("sha")
	commit, ok := resolveRefToCommit(repo, ref)
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	// newest-first walk (DFS pre-order), deduplicated
	var order []oid
	seen := make(map[oid]bool)
	stack := []oid{commit}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		obj := store.getObject(cur)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		seen[cur] = true
		order = append(order, cur)
		ci, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		for i := len(ci.Parents) - 1; i >= 0; i-- {
			stack = append(stack, ci.Parents[i])
		}
	}
	perPage, page := pageParams(r)
	start := (page - 1) * perPage
	if start > len(order) {
		start = len(order)
	}
	end := start + perPage
	if end > len(order) {
		end = len(order)
	}
	out := []any{}
	for _, c := range order[start:end] {
		out = append(out, commitJSON(c))
	}
	writeJSON(w, 200, out)
}

// ---- labels ----

func labelJSON(name, color string) map[string]any {
	return map[string]any{"name": name, "color": color, "default": false, "description": nil}
}

func restLabels(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) != 0 {
		writeErr(w, 404, "Not Found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		out := []any{}
		withRepoRead(repo, func() {
			for _, name := range repo.LabelOrder {
				out = append(out, labelJSON(name, repo.Labels[name]))
			}
		})
		writeJSON(w, 200, out)
	case http.MethodPost:
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		name, has := strField(body, "name")
		if !has || name == "" {
			writeErr(w, 422, "Validation Failed: name is required")
			return
		}
		color, _ := strField(body, "color")
		created := false
		withRepoWrite(repo, func() {
			if _, exists := repo.Labels[name]; !exists {
				repo.Labels[name] = color
				repo.LabelOrder = append(repo.LabelOrder, name)
				created = true
			}
		})
		if !created {
			writeErr(w, 422, "Validation Failed: label already exists")
			return
		}
		writeJSON(w, 201, labelJSON(name, color))
	default:
		writeErr(w, 404, "Not Found")
	}
}
