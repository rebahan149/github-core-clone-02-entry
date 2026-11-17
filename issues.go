package main

// Issues, comments and pull requests (REST), plus the shared merge engine the
// GraphQL mutations also use. Issue and PR numbers share one per-repo counter,
// as on GitHub.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---- serialisation ----

func issueLabelsJSON(repo *Repo, iss *Issue) []any {
	out := []any{}
	for _, name := range iss.Labels {
		out = append(out, map[string]any{"name": name, "color": repo.Labels[name]})
	}
	return out
}

func issueJSON(repo *Repo, iss *Issue) map[string]any {
	return map[string]any{
		"id":         iss.ID,
		"number":     iss.Number,
		"title":      iss.Title,
		"body":       iss.Body,
		"state":      iss.State,
		"labels":     issueLabelsJSON(repo, iss),
		"user":       map[string]any{"login": viewerLogin, "id": viewerID},
		"comments":   len(iss.Comments),
		"created_at": iss.CreatedAt,
		"updated_at": iss.UpdatedAt,
	}
}

// rebuildIssueCache refreshes the serialised REST form served by the hot read path.
// Call with repo.mu held.
func rebuildIssueCache(repo *Repo, iss *Issue) {
	data, _ := json.Marshal(issueJSON(repo, iss))
	iss.jsonCache = data
}

func commentJSON(c *Comment) map[string]any {
	return map[string]any{
		"id":         c.ID,
		"body":       c.Body,
		"user":       map[string]any{"login": viewerLogin, "id": viewerID},
		"created_at": c.CreatedAt,
		"updated_at": c.CreatedAt,
	}
}

// pullJSON serialises a PR. Call with repo.mu held (read or write): branch
// tips are read straight from the ref map.
func pullJSON(repo *Repo, p *Pull) map[string]any {
	headSHA := zeroOID
	if tip, ok := repo.Refs["refs/heads/"+p.Head]; ok {
		headSHA = tip
	}
	baseSHA := zeroOID
	if tip, ok := repo.Refs["refs/heads/"+p.Base]; ok {
		baseSHA = tip
	}
	var mergedAt any
	if p.Merged {
		mergedAt = p.MergedAt
	}
	var mergeSHA any
	if p.MergeSHA != "" {
		mergeSHA = p.MergeSHA
	}
	var mergeable any
	if p.State == "open" {
		_, conflicts := store.pullMergePreview(repo, p)
		mergeable = len(conflicts) == 0
	}
	return map[string]any{
		"id":               p.ID,
		"number":           p.Number,
		"state":            p.State,
		"title":            p.Title,
		"body":             p.Body,
		"head":             map[string]any{"ref": p.Head, "sha": headSHA.Hex(), "label": repo.Owner + ":" + p.Head},
		"base":             map[string]any{"ref": p.Base, "sha": baseSHA.Hex(), "label": repo.Owner + ":" + p.Base},
		"merged":           p.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"mergeable":        mergeable,
		"user":             map[string]any{"login": viewerLogin, "id": viewerID},
		"created_at":       p.CreatedAt,
		"updated_at":       p.UpdatedAt,
	}
}

// ---- shared creation helpers (used by REST, GraphQL and MCP) ----

// createIssueIn inserts an issue; call with repo.mu held. Returns nil when a
// label is unknown.
func createIssueIn(repo *Repo, title, body string, labels []string) *Issue {
	for _, name := range labels {
		if _, ok := repo.Labels[name]; !ok {
			return nil
		}
	}
	ts := store.now()
	iss := &Issue{
		ID:        store.nextID(),
		Number:    repo.NextNum,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    append([]string{}, labels...),
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	repo.NextNum++
	repo.Issues[iss.Number] = iss
	rebuildIssueCache(repo, iss)
	return iss
}

// createPullIn inserts a pull request; call with repo.mu held.
func createPullIn(repo *Repo, title, body, head, base string) *Pull {
	ts := store.now()
	p := &Pull{
		ID:        store.nextID(),
		Number:    repo.NextNum,
		Title:     title,
		Body:      body,
		Head:      head,
		Base:      base,
		State:     "open",
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	repo.NextNum++
	repo.Pulls[p.Number] = p
	return p
}

// ---- merge engine ----

// pullMergePreview computes the three-way tree merge for a PR without applying
// it. Returns the merged flat tree and the conflicting paths.
// Call with repo.mu held.
func (s *Store) pullMergePreview(repo *Repo, p *Pull) (map[string]treeEntry, []string) {
	baseTip, ok1 := repo.Refs["refs/heads/"+p.Base]
	headTip, ok2 := repo.Refs["refs/heads/"+p.Head]
	if !ok1 || !ok2 {
		return nil, []string{"<missing branch>"}
	}
	mb, ok := s.mergeBase(baseTip, headTip)
	if !ok {
		return nil, []string{"<unrelated histories>"}
	}
	flatOf := func(commit oid) map[string]treeEntry {
		tree, tok := commitTreeOf(commit)
		if !tok {
			return map[string]treeEntry{}
		}
		return s.flattenTree(tree)
	}
	merged, conflicts := mergeTrees(flatOf(mb), flatOf(baseTip), flatOf(headTip))
	return merged, conflicts
}

// mergePullSquash performs the squash merge: one new commit on the base tip
// carrying the merged tree. Call with repo.mu held. Returns (mergeSHA, conflicts).
func (s *Store) mergePullSquash(repo *Repo, p *Pull) (string, []string) {
	merged, conflicts := s.pullMergePreview(repo, p)
	if len(conflicts) > 0 {
		return "", conflicts
	}
	baseTip := repo.Refs["refs/heads/"+p.Base]
	newTree := s.buildTreeFromFlat(merged)
	ts := s.nowUnix()
	ident := viewerIdent(ts)
	msg := p.Title
	if p.Body != "" {
		msg += "\n\n" + p.Body
	}
	commitData := buildCommit(newTree, []oid{baseTip}, ident, ident, msg+"\n")
	commit := s.putObject("commit", commitData)
	repo.Refs["refs/heads/"+p.Base] = commit
	p.State = "closed"
	p.Merged = true
	p.MergeSHA = commit.Hex()
	p.MergedAt = s.now()
	p.UpdatedAt = p.MergedAt
	return commit.Hex(), nil
}

// pullFiles computes the changed-file list of a PR against its merge base.
// Call with repo.mu held.
func (s *Store) pullFiles(repo *Repo, p *Pull) []map[string]any {
	baseTip, ok1 := repo.Refs["refs/heads/"+p.Base]
	headTip, ok2 := repo.Refs["refs/heads/"+p.Head]
	if !ok1 || !ok2 {
		return []map[string]any{}
	}
	mb, ok := s.mergeBase(baseTip, headTip)
	if !ok {
		mb = baseTip
	}
	flatOf := func(commit oid) map[string]treeEntry {
		tree, tok := commitTreeOf(commit)
		if !tok {
			return map[string]treeEntry{}
		}
		return s.flattenTree(tree)
	}
	baseFlat := flatOf(mb)
	headFlat := flatOf(headTip)
	paths := make(map[string]bool)
	for pth := range baseFlat {
		paths[pth] = true
	}
	for pth := range headFlat {
		paths[pth] = true
	}
	var sorted []string
	for pth := range paths {
		sorted = append(sorted, pth)
	}
	sort.Strings(sorted)
	out := []map[string]any{}
	for _, pth := range sorted {
		b, bok := baseFlat[pth]
		h, hok := headFlat[pth]
		if bok && hok && b.Oid == h.Oid {
			continue
		}
		status := "modified"
		if !bok {
			status = "added"
		} else if !hok {
			status = "removed"
		}
		var oldLines, newLines []string
		if bok {
			if obj := s.getObject(b.Oid); obj != nil {
				oldLines = splitLines(string(obj.Data))
			}
		}
		if hok {
			if obj := s.getObject(h.Oid); obj != nil {
				newLines = splitLines(string(obj.Data))
			}
		}
		adds, dels := lineDiffCounts(oldLines, newLines)
		out = append(out, map[string]any{
			"filename":  pth,
			"status":    status,
			"additions": adds,
			"deletions": dels,
			"changes":   adds + dels,
		})
	}
	return out
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineDiffCounts returns (additions, deletions) between two line slices via an
// LCS dynamic program, with a naive fallback for very large inputs.
func lineDiffCounts(oldLines, newLines []string) (int, int) {
	n, m := len(oldLines), len(newLines)
	if n == 0 {
		return m, 0
	}
	if m == 0 {
		return 0, n
	}
	if n*m > 4_000_000 {
		return m, n
	}
	// LCS length with two rows
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if oldLines[i-1] == newLines[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] >= cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	lcs := prev[m]
	return m - lcs, n - lcs
}

// ---- REST: issues ----

func restIssues(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodPost:
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			title, _ := strField(body, "title")
			bodyText, _ := strField(body, "body")
			labels := labelNamesOf(body["labels"])
			var iss *Issue
			var fail bool
			withRepoWrite(repo, func() {
				iss = createIssueIn(repo, title, bodyText, labels)
				if iss == nil {
					fail = true
				}
			})
			if fail {
				writeErr(w, 422, "Validation Failed: unknown label")
				return
			}
			writeJSONBytes(w, 201, iss.jsonCache)
		case http.MethodGet:
			state := r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			direction := "asc"
			if s := r.URL.Query().Get("sort"); s == "created" {
				if d := r.URL.Query().Get("direction"); d == "desc" || d == "asc" {
					direction = d
				}
			}
			perPage, page := pageParams(r)
			var all []*Issue
			withRepoRead(repo, func() {
				for _, iss := range repo.Issues {
					if state == "all" || iss.State == state {
						all = append(all, iss)
					}
				}
			})
			sort.Slice(all, func(i, j int) bool {
				if direction == "desc" {
					return all[i].Number > all[j].Number
				}
				return all[i].Number < all[j].Number
			})
			start := (page - 1) * perPage
			if start > len(all) {
				start = len(all)
			}
			end := start + perPage
			if end > len(all) {
				end = len(all)
			}
			out := []any{}
			withRepoRead(repo, func() {
				for _, iss := range all[start:end] {
					out = append(out, issueJSON(repo, iss))
				}
			})
			writeJSON(w, 200, out)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}

	num, err := strconv.Atoi(rest[0])
	if err != nil {
		writeErr(w, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case http.MethodGet:
			var iss *Issue
			withRepoRead(repo, func() { iss = repo.Issues[num] })
			if iss == nil {
				writeErr(w, 404, "Not Found")
				return
			}
			// hot path: serve the cached serialisation
			writeJSONBytes(w, 200, iss.jsonCache)
		case http.MethodPatch:
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			var out map[string]any
			var found bool
			withRepoWrite(repo, func() {
				iss := repo.Issues[num]
				if iss == nil {
					return
				}
				found = true
				if v, has := strField(body, "state"); has && (v == "open" || v == "closed") {
					iss.State = v
				}
				if v, has := strField(body, "title"); has {
					iss.Title = v
				}
				if v, has := strField(body, "body"); has {
					iss.Body = v
				}
				if lv, present := body["labels"]; present {
					iss.Labels = labelNamesOf(lv)
				}
				iss.UpdatedAt = store.now()
				rebuildIssueCache(repo, iss)
				out = issueJSON(repo, iss)
			})
			if !found {
				writeErr(w, 404, "Not Found")
				return
			}
			writeJSON(w, 200, out)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}

	if len(rest) == 2 && rest[1] == "comments" {
		switch r.Method {
		case http.MethodPost:
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			text, _ := strField(body, "body")
			var out map[string]any
			var found bool
			withRepoWrite(repo, func() {
				iss := repo.Issues[num]
				if iss == nil {
					return
				}
				found = true
				c := &Comment{ID: store.nextID(), Body: text, CreatedAt: store.now()}
				iss.Comments = append(iss.Comments, c)
				iss.UpdatedAt = c.CreatedAt
				rebuildIssueCache(repo, iss)
				out = commentJSON(c)
			})
			if !found {
				writeErr(w, 404, "Not Found")
				return
			}
			writeJSON(w, 201, out)
		case http.MethodGet:
			var out []any
			var found bool
			withRepoRead(repo, func() {
				iss := repo.Issues[num]
				if iss == nil {
					return
				}
				found = true
				out = []any{}
				for _, c := range iss.Comments {
					out = append(out, commentJSON(c))
				}
			})
			if !found {
				writeErr(w, 404, "Not Found")
				return
			}
			writeJSON(w, 200, out)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	writeErr(w, 404, "Not Found")
}

func labelNamesOf(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		switch t := item.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			if name, ok := t["name"].(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

// ---- REST: pulls ----

func restPulls(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodPost:
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			title, _ := strField(body, "title")
			bodyText, _ := strField(body, "body")
			head, _ := strField(body, "head")
			base, _ := strField(body, "base")
			if i := strings.Index(head, ":"); i >= 0 {
				head = head[i+1:]
			}
			if i := strings.Index(base, ":"); i >= 0 {
				base = base[i+1:]
			}
			var out map[string]any
			var failMsg string
			withRepoWrite(repo, func() {
				if _, ok := repo.Refs["refs/heads/"+head]; !ok {
					failMsg = "head branch does not exist"
					return
				}
				if _, ok := repo.Refs["refs/heads/"+base]; !ok {
					failMsg = "base branch does not exist"
					return
				}
				if head == base {
					failMsg = "head and base are the same"
					return
				}
				p := createPullIn(repo, title, bodyText, head, base)
				out = pullJSON(repo, p)
			})
			if failMsg != "" {
				writeErr(w, 422, "Validation Failed: "+failMsg)
				return
			}
			writeJSON(w, 201, out)
		case http.MethodGet:
			state := r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			var all []*Pull
			withRepoRead(repo, func() {
				for _, p := range repo.Pulls {
					if state == "all" || p.State == state {
						all = append(all, p)
					}
				}
			})
			sort.Slice(all, func(i, j int) bool { return all[i].Number < all[j].Number })
			out := []any{}
			withRepoRead(repo, func() {
				for _, p := range all {
					out = append(out, pullJSON(repo, p))
				}
			})
			writeJSON(w, 200, out)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}

	num, err := strconv.Atoi(rest[0])
	if err != nil {
		writeErr(w, 404, "Not Found")
		return
	}
	var pull *Pull
	withRepoRead(repo, func() { pull = repo.Pulls[num] })
	if pull == nil {
		writeErr(w, 404, "Not Found")
		return
	}

	if len(rest) == 1 && r.Method == http.MethodGet {
		var out map[string]any
		withRepoRead(repo, func() { out = pullJSON(repo, pull) })
		writeJSON(w, 200, out)
		return
	}
	if len(rest) == 2 && rest[1] == "files" && r.Method == http.MethodGet {
		out := []any{}
		withRepoRead(repo, func() {
			for _, f := range store.pullFiles(repo, pull) {
				out = append(out, f)
			}
		})
		writeJSON(w, 200, out)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && r.Method == http.MethodPut {
		if _, ok := readJSONBody(w, r); !ok {
			return
		}
		var sha string
		var conflicts []string
		var already bool
		withRepoWrite(repo, func() {
			p := repo.Pulls[num]
			if p.Merged {
				already = true
				return
			}
			sha, conflicts = store.mergePullSquash(repo, p)
		})
		if already {
			writeErr(w, 405, "Pull Request is not mergeable: already merged")
			return
		}
		if len(conflicts) > 0 {
			writeErr(w, 405, "Pull Request is not mergeable: conflicts in "+strings.Join(conflicts, ", "))
			return
		}
		writeJSON(w, 200, map[string]any{
			"sha":     sha,
			"merged":  true,
			"message": "Pull Request successfully merged",
		})
		return
	}
	writeErr(w, 404, "Not Found")
}
