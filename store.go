package main

// The forge's in-memory store: one global content-addressed object map plus
// per-repository refs, labels, issues, pull requests and comments. All state is
// derived deterministically (a counter clock, sequential ids) so a live run and
// a replay produce identical responses.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
)

type Comment struct {
	ID        int
	Body      string
	CreatedAt string
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	Comments  []*Comment
	CreatedAt string
	UpdatedAt string
	jsonCache []byte // serialised REST form, rebuilt on every mutation
}

type Pull struct {
	ID        int
	Number    int
	Title     string
	Body      string
	Head      string // branch name
	Base      string // branch name
	State     string // "open" | "closed"
	Merged    bool
	MergeSHA  string
	MergedAt  string
	CreatedAt string
	UpdatedAt string
}

type Repo struct {
	mu            sync.RWMutex // guards this repo's refs, labels, issues, pulls
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	Refs          map[string]oid
	Labels        map[string]string // name -> color
	LabelOrder    []string
	Issues        map[int]*Issue
	Pulls         map[int]*Pull
	NextNum       int // shared issue/PR number counter (GitHub-style)
}

func (r *Repo) key() string { return r.Owner + "/" + r.Name }

type Store struct {
	mu        sync.RWMutex
	objects   map[oid]*gitObject
	repos     map[string]*Repo
	repoSeq   int
	idSeq     int // global id sequence for issues / PRs / comments
	clockSeq  int64
	packMu    sync.Mutex
	packCache map[string][]byte // fetch pack cache keyed by wants/depth/haves signature
}

func NewStore() *Store {
	return &Store{
		objects:   make(map[oid]*gitObject),
		repos:     make(map[string]*Repo),
		clockSeq:  1_700_000_000,
		packCache: make(map[string][]byte),
	}
}

// nowUnix is the deterministic clock: a fixed epoch plus a per-use increment.
func (s *Store) nowUnix() int64 {
	return atomic.AddInt64(&s.clockSeq, 1)
}

func (s *Store) now() string {
	return time.Unix(s.nowUnix(), 0).UTC().Format("2006-01-02T15:04:05Z")
}

func rfc3339(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// ---- objects ----

func (s *Store) putObject(typ string, data []byte) oid {
	id := hashObject(typ, data)
	s.mu.Lock()
	if _, ok := s.objects[id]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		s.objects[id] = &gitObject{Type: typ, Data: cp, Z: zlibCompress(cp)}
	}
	s.mu.Unlock()
	return id
}

func (s *Store) getObject(id oid) *gitObject {
	s.mu.RLock()
	obj := s.objects[id]
	s.mu.RUnlock()
	return obj
}

func (s *Store) hasObject(id oid) bool { return s.getObject(id) != nil }

// ---- repos ----

func (s *Store) createRepo(owner, name, desc string, priv bool) (*Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := owner + "/" + name
	if _, exists := s.repos[k]; exists {
		return nil, false
	}
	s.repoSeq++
	r := &Repo{
		ID:            s.repoSeq,
		Owner:         owner,
		Name:          name,
		Description:   desc,
		Private:       priv,
		DefaultBranch: "main",
		Refs:          make(map[string]oid),
		Labels:        make(map[string]string),
		Issues:        make(map[int]*Issue),
		Pulls:         make(map[int]*Pull),
		NextNum:       1,
	}
	s.repos[k] = r
	return r, true
}

func (s *Store) getRepo(owner, name string) *Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repos[owner+"/"+name]
}

func (s *Store) deleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := owner + "/" + name
	if _, ok := s.repos[k]; !ok {
		return false
	}
	delete(s.repos, k)
	return true
}

func (s *Store) listRepos(owner string) []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Repo
	for _, r := range s.repos {
		if r.Owner == owner {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) nextID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idSeq++
	return s.idSeq
}

// Per-repo locking: handlers hold the repo lock while touching repo state and
// take the store lock only briefly inside object helpers (lock order is always
// repo -> store, never the reverse).
func withRepoWrite(r *Repo, fn func()) { r.mu.Lock(); defer r.mu.Unlock(); fn() }
func withRepoRead(r *Repo, fn func())  { r.mu.RLock(); defer r.mu.RUnlock(); fn() }

// ---- ref helpers (self-locking; do NOT call while holding r.mu) ----

func (r *Repo) branchTip(branch string) (oid, bool) {
	return r.getRef("refs/heads/" + branch)
}

func (r *Repo) getRef(name string) (oid, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tip, ok := r.Refs[name]
	return tip, ok
}

func (r *Repo) setRef(name string, o oid) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Refs[name] = o
}

func (r *Repo) delRef(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.Refs, name)
}

func (r *Repo) refSnapshot() map[string]oid {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cp := make(map[string]oid, len(r.Refs))
	for k, v := range r.Refs {
		cp[k] = v
	}
	return cp
}

func (r *Repo) sortedRefNames() []string {
	r.mu.RLock()
	names := make([]string, 0, len(r.Refs))
	for n := range r.Refs {
		names = append(names, n)
	}
	r.mu.RUnlock()
	sort.Strings(names)
	return names
}

// ---- commit graph walks ----

// commitAncestors returns every commit reachable from start (including start).
func (s *Store) commitAncestors(start oid) map[oid]bool {
	seen := make(map[oid]bool)
	stack := []oid{start}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		obj := s.getObject(cur)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		seen[cur] = true
		ci, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		stack = append(stack, ci.Parents...)
	}
	return seen
}

func (s *Store) isAncestor(anc, desc oid) bool {
	if anc == desc {
		return true
	}
	// walk desc's ancestry
	seen := make(map[oid]bool)
	stack := []oid{desc}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := s.getObject(cur)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		ci, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		stack = append(stack, ci.Parents...)
	}
	return false
}

// mergeBase finds a best common ancestor of a and b (BFS from b over a's ancestor set).
func (s *Store) mergeBase(a, b oid) (oid, bool) {
	ancA := s.commitAncestors(a)
	if ancA[b] {
		return b, true
	}
	if ancA[a] && s.isAncestor(a, b) {
		return a, true
	}
	// BFS from b, generation by generation
	seen := make(map[oid]bool)
	frontier := []oid{b}
	for len(frontier) > 0 {
		var next []oid
		for _, cur := range frontier {
			if ancA[cur] {
				return cur, true
			}
			if seen[cur] {
				continue
			}
			seen[cur] = true
			obj := s.getObject(cur)
			if obj == nil || obj.Type != "commit" {
				continue
			}
			ci, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			next = append(next, ci.Parents...)
		}
		frontier = next
	}
	return zeroOID, false
}

// ---- tree flatten / rebuild ----

// flattenTree expands a tree object into path -> entry (files only).
func (s *Store) flattenTree(tree oid) map[string]treeEntry {
	out := make(map[string]treeEntry)
	var walk func(t oid, prefix string)
	walk = func(t oid, prefix string) {
		obj := s.getObject(t)
		if obj == nil || obj.Type != "tree" {
			return
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := prefix + e.Name
			if e.IsDir() {
				walk(e.Oid, p+"/")
			} else {
				out[p] = e
			}
		}
	}
	walk(tree, "")
	return out
}

// buildTreeFromFlat creates (and stores) nested trees for a flat path map; returns the root tree oid.
func (s *Store) buildTreeFromFlat(flat map[string]treeEntry) oid {
	type node struct {
		files map[string]treeEntry
		dirs  map[string]*node
	}
	root := &node{files: map[string]treeEntry{}, dirs: map[string]*node{}}
	for path, e := range flat {
		parts := strings.Split(path, "/")
		cur := root
		for _, d := range parts[:len(parts)-1] {
			if cur.dirs[d] == nil {
				cur.dirs[d] = &node{files: map[string]treeEntry{}, dirs: map[string]*node{}}
			}
			cur = cur.dirs[d]
		}
		cur.files[parts[len(parts)-1]] = e
	}
	var build func(n *node) oid
	build = func(n *node) oid {
		var entries []treeEntry
		for name, e := range n.files {
			entries = append(entries, treeEntry{Mode: e.Mode, Name: name, Oid: e.Oid})
		}
		for name, d := range n.dirs {
			entries = append(entries, treeEntry{Mode: "40000", Name: name, Oid: build(d)})
		}
		return s.putObject("tree", buildTree(entries))
	}
	return build(root)
}

// treeAtPath resolves a path inside a tree: returns the entry for the final component.
// kind is "file", "dir" or "" (missing). For a dir, entries lists its children.
func (s *Store) treeAtPath(tree oid, path string) (entry treeEntry, entries []treeEntry, kind string) {
	if path == "" || path == "." {
		obj := s.getObject(tree)
		if obj == nil {
			return entry, nil, ""
		}
		es, err := parseTree(obj.Data)
		if err != nil {
			return entry, nil, ""
		}
		return treeEntry{Mode: "40000", Name: "", Oid: tree}, es, "dir"
	}
	parts := strings.Split(path, "/")
	cur := tree
	for i, part := range parts {
		obj := s.getObject(cur)
		if obj == nil || obj.Type != "tree" {
			return entry, nil, ""
		}
		es, err := parseTree(obj.Data)
		if err != nil {
			return entry, nil, ""
		}
		var found *treeEntry
		for j := range es {
			if es[j].Name == part {
				found = &es[j]
				break
			}
		}
		if found == nil {
			return entry, nil, ""
		}
		if i == len(parts)-1 {
			if found.IsDir() {
				sub := s.getObject(found.Oid)
				if sub == nil {
					return entry, nil, ""
				}
				subEntries, err := parseTree(sub.Data)
				if err != nil {
					return entry, nil, ""
				}
				return *found, subEntries, "dir"
			}
			return *found, nil, "file"
		}
		if !found.IsDir() {
			return entry, nil, ""
		}
		cur = found.Oid
	}
	return entry, nil, ""
}

// ---- three-way tree merge ----

// mergeTrees merges flat maps: base (merge-base), ours, theirs.
// Returns the merged map and the list of conflicting paths.
func mergeTrees(base, ours, theirs map[string]treeEntry) (map[string]treeEntry, []string) {
	merged := make(map[string]treeEntry)
	var conflicts []string
	paths := make(map[string]bool)
	for p := range base {
		paths[p] = true
	}
	for p := range ours {
		paths[p] = true
	}
	for p := range theirs {
		paths[p] = true
	}
	same := func(a, b treeEntry, aok, bok bool) bool {
		if aok != bok {
			return false
		}
		if !aok {
			return true
		}
		return a.Oid == b.Oid && a.Mode == b.Mode
	}
	for p := range paths {
		b, bok := base[p]
		o, ook := ours[p]
		t, tok := theirs[p]
		switch {
		case same(o, t, ook, tok):
			if ook {
				merged[p] = o
			}
		case same(b, o, bok, ook): // only theirs changed
			if tok {
				merged[p] = t
			}
		case same(b, t, bok, tok): // only ours changed
			if ook {
				merged[p] = o
			}
		default:
			conflicts = append(conflicts, p)
		}
	}
	sort.Strings(conflicts)
	return merged, conflicts
}

// ---- fetch object selection ----

// objectClosure returns every object reachable from roots. When stopAtShallow is
// non-nil, commit traversal does not cross the parents of commits in that set
// (the client's declared shallow boundary, used for have-closure computation).
func (s *Store) objectClosure(roots []oid, stopAtShallow map[oid]bool) map[oid]bool {
	seen := make(map[oid]bool)
	stack := append([]oid{}, roots...)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		obj := s.getObject(cur)
		if obj == nil {
			continue
		}
		seen[cur] = true
		switch obj.Type {
		case "commit":
			ci, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			stack = append(stack, ci.Tree)
			if stopAtShallow != nil && stopAtShallow[cur] {
				continue // do not cross the shallow boundary
			}
			stack = append(stack, ci.Parents...)
		case "tree":
			entries, err := parseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				stack = append(stack, e.Oid)
			}
		case "tag":
			target, _, err := parseTagTarget(obj.Data)
			if err == nil {
				stack = append(stack, target)
			}
		}
	}
	return seen
}

type fetchPlan struct {
	Objects   []oid // objects to pack, commits first then trees/blobs/tags
	Shallow   []oid
	Unshallow []oid
	Empty     bool // nothing to send (wants already had)
}

// planFetch computes the object set for a v2 fetch.
func (s *Store) planFetch(wants, haves, clientShallow []oid, depth int, deepenSince int64, deepenNot []oid) fetchPlan {
	shallowSet := make(map[oid]bool)
	for _, c := range clientShallow {
		shallowSet[c] = true
	}
	haveClosure := s.objectClosure(haves, shallowSet)
	// commits the client provably has (for ACK purposes)
	plan := fetchPlan{}

	if depth > 0 || deepenSince > 0 {
		// generation-limited walk over commits from the wants
		type item struct {
			c   oid
			gen int
		}
		included := make(map[oid]bool)
		queue := []item{}
		for _, w := range wants {
			if obj := s.getObject(w); obj != nil && obj.Type == "commit" {
				queue = append(queue, item{w, 0})
			}
		}
		seenGen := make(map[oid]int)
		for len(queue) > 0 {
			it := queue[0]
			queue = queue[1:]
			if prev, ok := seenGen[it.c]; ok && prev <= it.gen {
				continue
			}
			seenGen[it.c] = it.gen
			obj := s.getObject(it.c)
			if obj == nil || obj.Type != "commit" {
				continue
			}
			ci, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			if deepenSince > 0 && depth == 0 && ci.CommitterTs < deepenSince {
				continue
			}
			included[it.c] = true
			for _, p := range ci.Parents {
				queue = append(queue, item{p, it.gen + 1})
			}
		}
		// deepen-not: drop anything reachable from those commits
		if len(deepenNot) > 0 {
			excluded := s.commitAncestorsMulti(deepenNot)
			for c := range excluded {
				delete(included, c)
			}
		}
		// boundary: included commit with a parent that exists but is not included
		boundary := make(map[oid]bool)
		for c := range included {
			obj := s.getObject(c)
			ci, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			for _, p := range ci.Parents {
				if s.hasObject(p) && !included[p] {
					boundary[c] = true
				}
			}
		}
		// depth limit: commits at generation depth-1 are the boundary; deeper gens excluded
		if depth > 0 {
			for c, g := range seenGen {
				if g >= depth {
					delete(included, c)
				}
			}
			// recompute boundary after trimming
			boundary = make(map[oid]bool)
			for c := range included {
				obj := s.getObject(c)
				ci, err := parseCommit(obj.Data)
				if err != nil {
					continue
				}
				for _, p := range ci.Parents {
					if s.hasObject(p) && !included[p] {
						boundary[c] = true
					}
				}
			}
		}
		for c := range boundary {
			plan.Shallow = append(plan.Shallow, c)
		}
		for _, c := range clientShallow {
			if included[c] && !boundary[c] {
				plan.Unshallow = append(plan.Unshallow, c)
			}
		}
		// full object set: included commits + each commit's own tree closure only
		// (never the trees of excluded ancestors), minus the have-closure
		all := make(map[oid]bool)
		for c := range included {
			all[c] = true
			obj := s.getObject(c)
			if obj == nil {
				continue
			}
			ci, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			stack := []oid{ci.Tree}
			for len(stack) > 0 {
				cur := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if all[cur] {
					continue
				}
				o := s.getObject(cur)
				if o == nil {
					continue
				}
				all[cur] = true
				if o.Type == "tree" {
					entries, err := parseTree(o.Data)
					if err != nil {
						continue
					}
					for _, e := range entries {
						stack = append(stack, e.Oid)
					}
				}
			}
		}
		plan.Objects = orderObjects(all, haveClosure, included)
	} else {
		wantClosure := s.objectClosure(wants, nil)
		plan.Objects = orderObjects(wantClosure, haveClosure, nil)
	}
	plan.Empty = len(plan.Objects) == 0
	sortOids(plan.Shallow)
	sortOids(plan.Unshallow)
	return plan
}

func (s *Store) commitAncestorsMulti(starts []oid) map[oid]bool {
	out := make(map[oid]bool)
	for _, st := range starts {
		for c := range s.commitAncestors(st) {
			out[c] = true
		}
	}
	return out
}

// orderObjects returns the set difference (all - have) in a stable order:
// commits first (in the given commit set order is not significant), then the rest.
func orderObjects(all, have map[oid]bool, commits map[oid]bool) []oid {
	var out []oid
	for o := range all {
		if have[o] {
			continue
		}
		out = append(out, o)
	}
	sortOids(out)
	return out
}

func sortOids(list []oid) {
	sort.Slice(list, func(i, j int) bool {
		return fmt.Sprintf("%x", list[i]) < fmt.Sprintf("%x", list[j])
	})
}
