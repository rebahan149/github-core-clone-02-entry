package main

// GraphQL executor: resolves the forge's schema (Query, Mutation, Repository,
// Issue, PullRequest, connections, __type introspection) against the store.
// Global ids are opaque base64 encodings of "<Type>:<parts>" and stable across
// reads and across the REST/GraphQL/MCP surfaces.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---- global ids ----

func globalID(kind string, parts ...int) string {
	var sb strings.Builder
	sb.WriteString(kind)
	for _, p := range parts {
		fmt.Fprintf(&sb, ":%d", p)
	}
	return base64.StdEncoding.EncodeToString([]byte(sb.String()))
}

func decodeGlobalID(s string) (string, []int, bool) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// tolerate URL-safe / unpadded encodings
		raw, err = base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			return "", nil, false
		}
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return "", nil, false
	}
	nums := make([]int, 0, len(parts)-1)
	for _, p := range parts[1:] {
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", nil, false
		}
		nums = append(nums, n)
	}
	return parts[0], nums, true
}

// ---- object model ----

type gqlObj struct {
	typ     string
	repo    *Repo
	iss     *Issue
	pull    *Pull
	comment *Comment
	extra   map[string]any
}

type gqlCtx struct {
	vars    map[string]any
	varDefs map[string]*gqlValue
	frags   map[string]*gqlFrag
	errs    []string
}

func (c *gqlCtx) eval(v *gqlValue) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case vString, vEnum:
		return v.s
	case vInt:
		n, _ := strconv.ParseInt(v.s, 10, 64)
		return int(n)
	case vFloat:
		f, _ := strconv.ParseFloat(v.s, 64)
		return f
	case vBool:
		return v.s == "true"
	case vNull:
		return nil
	case vList:
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, c.eval(item))
		}
		return out
	case vObject:
		out := make(map[string]any, len(v.obj))
		for _, kv := range v.obj {
			out[kv.k] = c.eval(kv.v)
		}
		return out
	case vVar:
		if val, ok := c.vars[v.s]; ok {
			return val
		}
		if def, ok := c.varDefs[v.s]; ok && def != nil {
			return c.eval(def)
		}
		return nil
	}
	return nil
}

func (c *gqlCtx) argStr(f *gqlField, name string) (string, bool) {
	v := f.arg(name)
	if v == nil {
		return "", false
	}
	s, ok := c.eval(v).(string)
	return s, ok
}

func (c *gqlCtx) argInt(f *gqlField, name string) (int, bool) {
	v := f.arg(name)
	if v == nil {
		return 0, false
	}
	return toInt(c.eval(v))
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case string:
		i, err := strconv.Atoi(n)
		return i, err == nil
	}
	return 0, false
}

func toStr(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func mapOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// evalList normalises an evaluated argument to a list (a lone scalar becomes
// a one-element list, GraphQL-style list coercion).
func evalList(v any) []any {
	if arr, ok := v.([]any); ok {
		return arr
	}
	if v == nil {
		return nil
	}
	return []any{v}
}

// collectFields expands fragments and merges duplicate response keys.
func (c *gqlCtx) collectFields(sel []*gqlField, typeName string) []*gqlField {
	var out []*gqlField
	index := map[string]int{}
	var add func(fields []*gqlField)
	add = func(fields []*gqlField) {
		for _, f := range fields {
			switch {
			case f.spread != "":
				if frag, ok := c.frags[f.spread]; ok && (frag.on == typeName || frag.on == "Node") {
					add(frag.sel)
				}
			case f.inlineOn != "":
				if f.inlineOn == typeName || f.inlineOn == "Node" {
					add(f.sel)
				}
			default:
				k := f.key()
				if i, exists := index[k]; exists {
					merged := *out[i]
					merged.sel = append(append([]*gqlField{}, out[i].sel...), f.sel...)
					out[i] = &merged
				} else {
					index[k] = len(out)
					out = append(out, f)
				}
			}
		}
	}
	add(sel)
	return out
}

func (c *gqlCtx) execObj(obj gqlObj, sel []*gqlField) map[string]any {
	out := map[string]any{}
	for _, f := range c.collectFields(sel, obj.typ) {
		if f.name == "__typename" {
			out[f.key()] = obj.typ
			continue
		}
		val, known := c.resolveField(obj, f)
		if !known {
			c.errs = append(c.errs, fmt.Sprintf("Field '%s' doesn't exist on type '%s'", f.name, obj.typ))
			out[f.key()] = nil
			continue
		}
		out[f.key()] = c.execValue(val, f.sel)
	}
	return out
}

func (c *gqlCtx) execValue(val any, sel []*gqlField) any {
	switch v := val.(type) {
	case nil:
		return nil
	case gqlObj:
		return c.execObj(v, sel)
	case []gqlObj:
		out := make([]any, 0, len(v))
		for _, o := range v {
			out = append(out, c.execObj(o, sel))
		}
		return out
	default:
		return val
	}
}

// ---- schema for __type ----

var gqlTypeFields = map[string][]string{
	"Query":                     {"viewer", "repository", "node", "search"},
	"Mutation":                  {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":                      {"id", "databaseId", "login", "name", "email"},
	"Repository":                {"id", "databaseId", "name", "nameWithOwner", "owner", "description", "isPrivate", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests", "createdAt", "updatedAt", "url"},
	"Issue":                     {"id", "databaseId", "number", "title", "state", "body", "repository", "author", "comments", "labels", "createdAt", "updatedAt", "url"},
	"PullRequest":               {"id", "databaseId", "number", "title", "body", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "repository", "author", "createdAt", "updatedAt", "url"},
	"IssueComment":              {"id", "databaseId", "body", "author", "createdAt", "updatedAt"},
	"Ref":                       {"name", "target"},
	"Commit":                    {"oid"},
	"PageInfo":                  {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"IssueConnection":           {"nodes", "pageInfo", "totalCount"},
	"PullRequestConnection":     {"nodes", "pageInfo", "totalCount"},
	"PullRequestFileConnection": {"nodes", "pageInfo", "totalCount"},
	"PullRequestChangedFile":    {"path"},
	"CreateIssuePayload":        {"issue", "clientMutationId"},
	"CloseIssuePayload":         {"issue", "clientMutationId"},
	"CreatePullRequestPayload":  {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":   {"pullRequest", "clientMutationId"},
	"AddCommentPayload":         {"commentEdge", "subject", "clientMutationId"},
	"IssueCommentEdge":          {"node", "cursor"},
	"__Type":                    {"name", "kind", "description", "fields", "enumValues", "possibleTypes", "inputFields", "ofType"},
	"__Field":                   {"name", "description", "type", "isDeprecated"},
	"__EnumValue":               {"name", "description", "isDeprecated"},
}

var gqlEnumValues = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PermissionV2":           {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"OrderDirection":         {"ASC", "DESC"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
}

// ---- resolvers ----

func userObj(login string) gqlObj {
	return gqlObj{typ: "User", extra: map[string]any{"login": login}}
}

func repoObj(repo *Repo) gqlObj { return gqlObj{typ: "Repository", repo: repo} }

func issueObj(repo *Repo, iss *Issue) gqlObj { return gqlObj{typ: "Issue", repo: repo, iss: iss} }

func pullObj(repo *Repo, p *Pull) gqlObj { return gqlObj{typ: "PullRequest", repo: repo, pull: p} }

func (c *gqlCtx) resolveField(obj gqlObj, f *gqlField) (any, bool) {
	switch obj.typ {
	case "Query":
		return c.resolveQuery(f)
	case "Mutation":
		return c.resolveMutation(f)
	case "User":
		return resolveUser(obj, f)
	case "Repository":
		return c.resolveRepository(obj, f)
	case "Issue":
		return resolveIssue(obj, f)
	case "PullRequest":
		return c.resolvePullRequest(obj, f)
	case "IssueComment":
		return resolveComment(obj, f)
	case "Ref":
		switch f.name {
		case "name":
			return obj.extra["name"], true
		case "target":
			return gqlObj{typ: "Commit", extra: map[string]any{"oid": obj.extra["oid"]}}, true
		}
	case "Commit":
		if f.name == "oid" {
			if o, ok := obj.extra["oid"].(oid); ok {
				return o.Hex(), true
			}
			return obj.extra["oid"], true
		}
	case "PageInfo":
		return obj.extra[f.name], true
	case "IssueConnection", "PullRequestConnection", "PullRequestFileConnection":
		switch f.name {
		case "nodes":
			return obj.extra["nodes"], true
		case "pageInfo":
			return gqlObj{typ: "PageInfo", extra: obj.extra["pageInfo"].(map[string]any)}, true
		case "totalCount":
			return obj.extra["totalCount"], true
		}
	case "PullRequestChangedFile":
		if f.name == "path" {
			return obj.extra["path"], true
		}
	case "CreateIssuePayload", "CloseIssuePayload":
		if f.name == "issue" {
			return obj.extra["issue"], true
		}
		if f.name == "clientMutationId" {
			return nil, true
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if f.name == "pullRequest" {
			return obj.extra["pullRequest"], true
		}
		if f.name == "clientMutationId" {
			return nil, true
		}
	case "AddCommentPayload":
		switch f.name {
		case "commentEdge":
			return obj.extra["commentEdge"], true
		case "subject":
			return obj.extra["subject"], true
		case "clientMutationId":
			return nil, true
		}
	case "IssueCommentEdge":
		switch f.name {
		case "node":
			return obj.extra["node"], true
		case "cursor":
			return "", true
		}
	case "__Type":
		return resolveTypeInfo(obj, f)
	case "__Field", "__EnumValue":
		if f.name == "name" {
			return obj.extra["name"], true
		}
		if f.name == "description" || f.name == "isDeprecated" {
			return nil, true
		}
		if f.name == "type" {
			return nil, true
		}
	}
	// generic fallback: plain data carried in extra (Label.name, edge fields, ...)
	if v, ok := obj.extra[f.name]; ok {
		return v, true
	}
	return nil, false
}

func resolveUser(obj gqlObj, f *gqlField) (any, bool) {
	login, _ := obj.extra["login"].(string)
	if login == "" {
		login = viewerLogin
	}
	switch f.name {
	case "login":
		return login, true
	case "id":
		return globalID("User", viewerID), true
	case "databaseId":
		return viewerID, true
	case "name":
		if login == viewerLogin {
			return viewerName, true
		}
		return login, true
	case "email":
		if login == viewerLogin {
			return viewerEmail, true
		}
		return login + "@example.invalid", true
	}
	return nil, false
}

func resolveTypeInfo(obj gqlObj, f *gqlField) (any, bool) {
	name, _ := obj.extra["name"].(string)
	switch f.name {
	case "name":
		return name, true
	case "kind":
		if _, isEnum := gqlEnumValues[name]; isEnum {
			return "ENUM", true
		}
		return "OBJECT", true
	case "description":
		return nil, true
	case "fields":
		fields, ok := gqlTypeFields[name]
		if !ok {
			return nil, true
		}
		out := []gqlObj{}
		for _, fn := range fields {
			out = append(out, gqlObj{typ: "__Field", extra: map[string]any{"name": fn}})
		}
		return out, true
	case "enumValues":
		vals, ok := gqlEnumValues[name]
		if !ok {
			return nil, true
		}
		out := []gqlObj{}
		for _, v := range vals {
			out = append(out, gqlObj{typ: "__EnumValue", extra: map[string]any{"name": v}})
		}
		return out, true
	case "possibleTypes", "inputFields", "ofType":
		return nil, true
	}
	return nil, false
}

func (c *gqlCtx) resolveQuery(f *gqlField) (any, bool) {
	switch f.name {
	case "viewer":
		return userObj(viewerLogin), true
	case "repository":
		owner, _ := c.argStr(f, "owner")
		name, _ := c.argStr(f, "name")
		repo := store.getRepo(owner, name)
		if repo == nil {
			return nil, true
		}
		return repoObj(repo), true
	case "node":
		idStr, _ := c.argStr(f, "id")
		return c.nodeByID(idStr), true
	case "__type":
		name, _ := c.argStr(f, "name")
		return gqlObj{typ: "__Type", extra: map[string]any{"name": name}}, true
	case "__schema":
		return nil, true
	}
	return nil, false
}

func (c *gqlCtx) nodeByID(idStr string) any {
	kind, nums, ok := decodeGlobalID(idStr)
	if !ok {
		return nil
	}
	switch kind {
	case "User":
		return userObj(viewerLogin)
	case "Repository":
		if len(nums) >= 1 {
			if repo := store.repoByID(nums[0]); repo != nil {
				return repoObj(repo)
			}
		}
	case "Issue":
		if len(nums) >= 2 {
			if repo := store.repoByID(nums[0]); repo != nil {
				var out any
				withRepoRead(repo, func() {
					if iss := repo.Issues[nums[1]]; iss != nil {
						out = issueObj(repo, iss)
					}
				})
				return out
			}
		}
	case "PullRequest":
		if len(nums) >= 2 {
			if repo := store.repoByID(nums[0]); repo != nil {
				var out any
				withRepoRead(repo, func() {
					if p := repo.Pulls[nums[1]]; p != nil {
						out = pullObj(repo, p)
					}
				})
				return out
			}
		}
	}
	return nil
}

func (s *Store) repoByID(id int) *Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (c *gqlCtx) resolveRepository(obj gqlObj, f *gqlField) (any, bool) {
	repo := obj.repo
	switch f.name {
	case "id":
		return globalID("Repository", repo.ID), true
	case "databaseId":
		return repo.ID, true
	case "name":
		return repo.Name, true
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name, true
	case "owner":
		return userObj(repo.Owner), true
	case "description":
		return repo.Description, true
	case "isPrivate":
		return repo.Private, true
	case "viewerPermission":
		return "ADMIN", true
	case "hasIssuesEnabled":
		return true, true
	case "createdAt", "updatedAt":
		return "2026-01-01T00:00:00Z", true
	case "url":
		return "https://forge.local/" + repo.Owner + "/" + repo.Name, true
	case "defaultBranchRef":
		tip, ok := repo.branchTip(repo.DefaultBranch)
		if !ok {
			return nil, true
		}
		return gqlObj{typ: "Ref", extra: map[string]any{"name": repo.DefaultBranch, "oid": tip}}, true
	case "issue":
		num, ok := c.argInt(f, "number")
		if !ok {
			return nil, true
		}
		var out any
		withRepoRead(repo, func() {
			if iss := repo.Issues[num]; iss != nil {
				out = issueObj(repo, iss)
			}
		})
		return out, true
	case "issues":
		return c.issuesConnection(repo, f), true
	case "pullRequest":
		num, ok := c.argInt(f, "number")
		if !ok {
			return nil, true
		}
		var out any
		withRepoRead(repo, func() {
			if p := repo.Pulls[num]; p != nil {
				out = pullObj(repo, p)
			}
		})
		return out, true
	case "pullRequests":
		return c.pullsConnection(repo, f), true
	}
	return nil, false
}

func resolveIssue(obj gqlObj, f *gqlField) (any, bool) {
	iss := obj.iss
	switch f.name {
	case "id":
		return globalID("Issue", obj.repo.ID, iss.Number), true
	case "databaseId":
		return iss.ID, true
	case "number":
		return iss.Number, true
	case "title":
		return iss.Title, true
	case "body":
		return iss.Body, true
	case "state":
		return strings.ToUpper(iss.State), true
	case "repository":
		return repoObj(obj.repo), true
	case "author":
		return userObj(viewerLogin), true
	case "createdAt":
		return iss.CreatedAt, true
	case "updatedAt":
		return iss.UpdatedAt, true
	case "url":
		return fmt.Sprintf("https://forge.local/%s/%s/issues/%d", obj.repo.Owner, obj.repo.Name, iss.Number), true
	case "comments":
		nodes := []gqlObj{}
		for _, cm := range iss.Comments {
			nodes = append(nodes, gqlObj{typ: "IssueComment", repo: obj.repo, iss: iss, comment: cm})
		}
		return connectionOf(nodes, len(nodes), 0), true
	case "labels":
		nodes := []gqlObj{}
		for _, name := range iss.Labels {
			nodes = append(nodes, gqlObj{typ: "Label", extra: map[string]any{"name": name}})
		}
		return connectionOf(nodes, len(nodes), 0), true
	}
	return nil, false
}

func resolveComment(obj gqlObj, f *gqlField) (any, bool) {
	cm := obj.comment
	switch f.name {
	case "id":
		return globalID("IssueComment", cm.ID), true
	case "databaseId":
		return cm.ID, true
	case "body":
		return cm.Body, true
	case "author":
		return userObj(viewerLogin), true
	case "createdAt":
		return cm.CreatedAt, true
	case "updatedAt":
		return cm.CreatedAt, true
	}
	return nil, false
}

func (c *gqlCtx) resolvePullRequest(obj gqlObj, f *gqlField) (any, bool) {
	p := obj.pull
	repo := obj.repo
	switch f.name {
	case "id":
		return globalID("PullRequest", repo.ID, p.Number), true
	case "databaseId":
		return p.ID, true
	case "number":
		return p.Number, true
	case "title":
		return p.Title, true
	case "body":
		return p.Body, true
	case "state":
		if p.Merged {
			return "MERGED", true
		}
		return strings.ToUpper(p.State), true
	case "merged":
		return p.Merged, true
	case "headRefName":
		return p.Head, true
	case "baseRefName":
		return p.Base, true
	case "repository":
		return repoObj(repo), true
	case "author":
		return userObj(viewerLogin), true
	case "createdAt":
		return p.CreatedAt, true
	case "updatedAt":
		return p.UpdatedAt, true
	case "url":
		return fmt.Sprintf("https://forge.local/%s/%s/pull/%d", repo.Owner, repo.Name, p.Number), true
	case "mergeable":
		var state string
		withRepoRead(repo, func() {
			switch {
			case p.Merged:
				state = "MERGEABLE"
			case p.State != "open":
				state = "UNKNOWN"
			default:
				_, conflicts := store.pullMergePreview(repo, p)
				if len(conflicts) > 0 {
					state = "CONFLICTING"
				} else {
					state = "MERGEABLE"
				}
			}
		})
		return state, true
	case "files":
		var nodes []gqlObj
		withRepoRead(repo, func() {
			for _, file := range store.pullFiles(repo, p) {
				nodes = append(nodes, gqlObj{typ: "PullRequestChangedFile", extra: map[string]any{"path": file["filename"]}})
			}
		})
		if nodes == nil {
			nodes = []gqlObj{}
		}
		first, hasFirst := c.argInt(f, "first")
		total := len(nodes)
		if hasFirst && first < len(nodes) {
			nodes = nodes[:first]
		}
		return gqlObj{typ: "PullRequestFileConnection", extra: map[string]any{
			"nodes":      nodes,
			"totalCount": total,
			"pageInfo":   map[string]any{"hasNextPage": hasFirst && first < total, "hasPreviousPage": false, "startCursor": nil, "endCursor": nil},
		}}, true
	}
	return nil, false
}

// ---- connections ----

func cursorOf(number int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", number)))
}

func numberOfCursor(s string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0, false
	}
	if !strings.HasPrefix(string(raw), "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(raw), "cursor:"))
	return n, err == nil
}

// paginate slices an ordered node list by first/after; numbers give cursor ids.
func paginate(nodes []gqlObj, numbers []int, first int, after string) gqlObj {
	start := 0
	if after != "" {
		if n, ok := numberOfCursor(after); ok {
			for i, num := range numbers {
				if num == n {
					start = i + 1
				}
			}
		}
	}
	total := len(nodes)
	end := total
	if first >= 0 && start+first < end {
		end = start + first
	}
	page := nodes[start:end]
	var endCursor any
	if len(page) > 0 {
		endCursor = cursorOf(numbers[end-1])
	}
	return gqlObj{typ: "IssueConnection", extra: map[string]any{
		"nodes":      page,
		"totalCount": total,
		"pageInfo": map[string]any{
			"hasNextPage":     end < total,
			"hasPreviousPage": start > 0,
			"startCursor":     nil,
			"endCursor":       endCursor,
		},
	}}
}

func connectionOf(nodes []gqlObj, total, _ int) gqlObj {
	if nodes == nil {
		nodes = []gqlObj{}
	}
	return gqlObj{typ: "IssueConnection", extra: map[string]any{
		"nodes":      nodes,
		"totalCount": total,
		"pageInfo":   map[string]any{"hasNextPage": false, "hasPreviousPage": false, "startCursor": nil, "endCursor": nil},
	}}
}

func (c *gqlCtx) issuesConnection(repo *Repo, f *gqlField) gqlObj {
	first, hasFirst := c.argInt(f, "first")
	if !hasFirst {
		first = 100
	}
	after, _ := c.argStr(f, "after")
	// states filter
	var states map[string]bool
	if v := f.arg("states"); v != nil {
		states = map[string]bool{}
		for _, item := range evalList(c.eval(v)) {
			if s, ok := item.(string); ok {
				states[strings.ToLower(s)] = true
			}
		}
	}
	desc := false
	if v := f.arg("orderBy"); v != nil {
		if m := mapOf(c.eval(v)); m != nil {
			if d, ok := m["direction"].(string); ok && d == "DESC" {
				desc = true
			}
		}
	}
	var nodes []gqlObj
	var numbers []int
	withRepoRead(repo, func() {
		var list []*Issue
		for _, iss := range repo.Issues {
			if states != nil && !states[iss.State] {
				continue
			}
			list = append(list, iss)
		}
		sort.Slice(list, func(i, j int) bool {
			if desc {
				return list[i].Number > list[j].Number
			}
			return list[i].Number < list[j].Number
		})
		for _, iss := range list {
			nodes = append(nodes, issueObj(repo, iss))
			numbers = append(numbers, iss.Number)
		}
	})
	if nodes == nil {
		nodes = []gqlObj{}
	}
	return paginate(nodes, numbers, first, after)
}

func (c *gqlCtx) pullsConnection(repo *Repo, f *gqlField) gqlObj {
	first, hasFirst := c.argInt(f, "first")
	if !hasFirst {
		first = 100
	}
	after, _ := c.argStr(f, "after")
	var states map[string]bool
	if v := f.arg("states"); v != nil {
		states = map[string]bool{}
		for _, item := range evalList(c.eval(v)) {
			if s, ok := item.(string); ok {
				states[s] = true
			}
		}
	} else {
		states = map[string]bool{"OPEN": true}
	}
	var nodes []gqlObj
	var numbers []int
	withRepoRead(repo, func() {
		var list []*Pull
		for _, p := range repo.Pulls {
			st := strings.ToUpper(p.State)
			if p.Merged {
				st = "MERGED"
			}
			if !states[st] {
				continue
			}
			list = append(list, p)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
		for _, p := range list {
			nodes = append(nodes, pullObj(repo, p))
			numbers = append(numbers, p.Number)
		}
	})
	if nodes == nil {
		nodes = []gqlObj{}
	}
	conn := paginate(nodes, numbers, first, after)
	conn.typ = "PullRequestConnection"
	return conn
}

// ---- mutations ----

// mutationInput returns the input object for a mutation field, accepting
// either an `input:` argument or direct arguments.
func (c *gqlCtx) mutationInput(f *gqlField) map[string]any {
	if v := f.arg("input"); v != nil {
		if m := mapOf(c.eval(v)); m != nil {
			return m
		}
	}
	out := map[string]any{}
	for _, kv := range f.args {
		out[kv.k] = c.eval(kv.v)
	}
	return out
}

func (c *gqlCtx) resolveMutation(f *gqlField) (any, bool) {
	input := c.mutationInput(f)
	switch f.name {
	case "createIssue":
		repoIDStr, _ := toStr(input["repositoryId"])
		kind, nums, ok := decodeGlobalID(repoIDStr)
		if !ok || kind != "Repository" || len(nums) < 1 {
			c.errs = append(c.errs, "createIssue: repositoryId is not a repository id")
			return gqlObj{typ: "CreateIssuePayload", extra: map[string]any{"issue": nil}}, true
		}
		repo := store.repoByID(nums[0])
		if repo == nil {
			c.errs = append(c.errs, "createIssue: repository not found")
			return gqlObj{typ: "CreateIssuePayload", extra: map[string]any{"issue": nil}}, true
		}
		title, _ := toStr(input["title"])
		bodyText, _ := toStr(input["body"])
		var iss *Issue
		withRepoWrite(repo, func() {
			iss = createIssueIn(repo, title, bodyText, nil)
		})
		if iss == nil {
			return gqlObj{typ: "CreateIssuePayload", extra: map[string]any{"issue": nil}}, true
		}
		return gqlObj{typ: "CreateIssuePayload", extra: map[string]any{"issue": issueObj(repo, iss)}}, true

	case "closeIssue":
		idStr, _ := toStr(input["issueId"])
		kind, nums, ok := decodeGlobalID(idStr)
		if !ok || kind != "Issue" || len(nums) < 2 {
			c.errs = append(c.errs, "closeIssue: issueId is not an issue id")
			return gqlObj{typ: "CloseIssuePayload", extra: map[string]any{"issue": nil}}, true
		}
		repo := store.repoByID(nums[0])
		if repo == nil {
			return gqlObj{typ: "CloseIssuePayload", extra: map[string]any{"issue": nil}}, true
		}
		var iss *Issue
		withRepoWrite(repo, func() {
			iss = repo.Issues[nums[1]]
			if iss != nil {
				iss.State = "closed"
				iss.UpdatedAt = store.now()
				rebuildIssueCache(repo, iss)
			}
		})
		if iss == nil {
			return gqlObj{typ: "CloseIssuePayload", extra: map[string]any{"issue": nil}}, true
		}
		return gqlObj{typ: "CloseIssuePayload", extra: map[string]any{"issue": issueObj(repo, iss)}}, true

	case "createPullRequest":
		repoIDStr, _ := toStr(input["repositoryId"])
		kind, nums, ok := decodeGlobalID(repoIDStr)
		if !ok || kind != "Repository" || len(nums) < 1 {
			c.errs = append(c.errs, "createPullRequest: repositoryId is not a repository id")
			return gqlObj{typ: "CreatePullRequestPayload", extra: map[string]any{"pullRequest": nil}}, true
		}
		repo := store.repoByID(nums[0])
		if repo == nil {
			return gqlObj{typ: "CreatePullRequestPayload", extra: map[string]any{"pullRequest": nil}}, true
		}
		title, _ := toStr(input["title"])
		bodyText, _ := toStr(input["body"])
		base, _ := toStr(input["baseRefName"])
		head, _ := toStr(input["headRefName"])
		var p *Pull
		withRepoWrite(repo, func() {
			if _, ok := repo.Refs["refs/heads/"+head]; !ok {
				return
			}
			if _, ok := repo.Refs["refs/heads/"+base]; !ok {
				return
			}
			p = createPullIn(repo, title, bodyText, head, base)
		})
		if p == nil {
			c.errs = append(c.errs, "createPullRequest: head or base branch not found")
			return gqlObj{typ: "CreatePullRequestPayload", extra: map[string]any{"pullRequest": nil}}, true
		}
		return gqlObj{typ: "CreatePullRequestPayload", extra: map[string]any{"pullRequest": pullObj(repo, p)}}, true

	case "mergePullRequest":
		idStr, _ := toStr(input["pullRequestId"])
		kind, nums, ok := decodeGlobalID(idStr)
		if !ok || kind != "PullRequest" || len(nums) < 2 {
			c.errs = append(c.errs, "mergePullRequest: pullRequestId is not a pull request id")
			return gqlObj{typ: "MergePullRequestPayload", extra: map[string]any{"pullRequest": nil}}, true
		}
		repo := store.repoByID(nums[0])
		if repo == nil {
			return gqlObj{typ: "MergePullRequestPayload", extra: map[string]any{"pullRequest": nil}}, true
		}
		var p *Pull
		var conflicts []string
		withRepoWrite(repo, func() {
			p = repo.Pulls[nums[1]]
			if p == nil || p.Merged {
				return
			}
			_, conflicts = store.mergePullSquash(repo, p)
		})
		if p == nil {
			return gqlObj{typ: "MergePullRequestPayload", extra: map[string]any{"pullRequest": nil}}, true
		}
		if len(conflicts) > 0 {
			c.errs = append(c.errs, "mergePullRequest: pull request has conflicts")
		}
		return gqlObj{typ: "MergePullRequestPayload", extra: map[string]any{"pullRequest": pullObj(repo, p)}}, true

	case "addComment":
		idStr, _ := toStr(input["subjectId"])
		kind, nums, ok := decodeGlobalID(idStr)
		if !ok || kind != "Issue" || len(nums) < 2 {
			c.errs = append(c.errs, "addComment: subjectId is not an issue id")
			return gqlObj{typ: "AddCommentPayload", extra: map[string]any{"commentEdge": nil}}, true
		}
		repo := store.repoByID(nums[0])
		if repo == nil {
			return gqlObj{typ: "AddCommentPayload", extra: map[string]any{"commentEdge": nil}}, true
		}
		bodyText, _ := toStr(input["body"])
		var edge gqlObj
		var iss *Issue
		withRepoWrite(repo, func() {
			iss = repo.Issues[nums[1]]
			if iss == nil {
				return
			}
			cm := &Comment{ID: store.nextID(), Body: bodyText, CreatedAt: store.now()}
			iss.Comments = append(iss.Comments, cm)
			iss.UpdatedAt = cm.CreatedAt
			rebuildIssueCache(repo, iss)
			edge = gqlObj{typ: "IssueCommentEdge", extra: map[string]any{"node": gqlObj{typ: "IssueComment", repo: repo, iss: iss, comment: cm}}}
		})
		if iss == nil {
			return gqlObj{typ: "AddCommentPayload", extra: map[string]any{"commentEdge": nil}}, true
		}
		return gqlObj{typ: "AddCommentPayload", extra: map[string]any{"commentEdge": edge, "subject": issueObj(repo, iss)}}, true
	}
	return nil, false
}

// ---- HTTP handler ----

func handleGraphQL(w http.ResponseWriter, r *http.Request) {
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
		writeErr(w, 400, "could not read body")
		return
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		writeErr(w, 400, "malformed JSON body")
		return
	}
	if batch, ok := decoded.([]any); ok {
		results := make([]any, 0, len(batch))
		for _, item := range batch {
			m, _ := item.(map[string]any)
			results = append(results, executeGraphQLRequest(m))
		}
		writeJSON(w, 200, results)
		return
	}
	m, _ := decoded.(map[string]any)
	writeJSON(w, 200, executeGraphQLRequest(m))
}

func executeGraphQLRequest(req map[string]any) map[string]any {
	query, _ := req["query"].(string)
	if query == "" {
		return map[string]any{"errors": []any{map[string]any{"message": "query is required"}}}
	}
	doc, err := gqlParse(query)
	if err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": "parse error: " + err.Error()}}}
	}
	vars := map[string]any{}
	if v, ok := req["variables"].(map[string]any); ok {
		vars = v
	}
	opName, _ := req["operationName"].(string)
	var op *gqlOp
	for _, candidate := range doc.ops {
		if opName == "" || candidate.name == opName {
			op = candidate
			break
		}
	}
	if op == nil {
		return map[string]any{"errors": []any{map[string]any{"message": "operation not found"}}}
	}
	ctx := &gqlCtx{vars: vars, varDefs: op.varDefs, frags: doc.frags}
	rootType := "Query"
	if op.typ == "mutation" {
		rootType = "Mutation"
	}
	data := ctx.execObj(gqlObj{typ: rootType}, op.sel)
	resp := map[string]any{"data": data}
	if len(ctx.errs) > 0 {
		errs := make([]any, 0, len(ctx.errs))
		for _, e := range ctx.errs {
			errs = append(errs, map[string]any{"message": e})
		}
		resp["errors"] = errs
	}
	return resp
}
