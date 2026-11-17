package main

// Git smart HTTP: protocol v2 (ls-refs, fetch) and v0 (ref advertisement,
// receive-pack with report-status). Pkt-line framing throughout; fetch packs
// are complete (never thin) and built from the object store, with a cache for
// the repeated depth-limited clone shape.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---- pkt-line ----

func pktLine(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(payload)+4))...)
	out = append(out, payload...)
	return out
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

func pktText(s string) []byte { return pktLine([]byte(s)) }

const (
	pktData = iota
	pktFlushKind
	pktDelimKind
)

type pktReader struct {
	data []byte
	pos  int
	err  error
}

// next returns (payload, kind). At end of input it returns kind pktFlushKind.
func (p *pktReader) next() ([]byte, int) {
	if p.pos >= len(p.data) {
		return nil, pktFlushKind
	}
	if len(p.data)-p.pos < 4 {
		p.err = fmt.Errorf("truncated pkt-line header")
		return nil, pktFlushKind
	}
	n, err := strconv.ParseUint(string(p.data[p.pos:p.pos+4]), 16, 32)
	if err != nil {
		p.err = fmt.Errorf("bad pkt-line length")
		return nil, pktFlushKind
	}
	switch n {
	case 0:
		p.pos += 4
		return nil, pktFlushKind
	case 1:
		p.pos += 4
		return nil, pktDelimKind
	case 2:
		p.pos += 4
		return nil, pktDelimKind // response-end: treat like a delimiter
	}
	if n < 4 || p.pos+int(n) > len(p.data) {
		p.err = fmt.Errorf("pkt-line overruns body")
		return nil, pktFlushKind
	}
	payload := p.data[p.pos+4 : p.pos+int(n)]
	p.pos += int(n)
	return payload, pktData
}

func (p *pktReader) rest() []byte {
	if p.pos >= len(p.data) {
		return nil
	}
	return p.data[p.pos:]
}

// ---- routing ----

func handleGitHTTP(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		writeErr(w, 401, "Bad credentials")
		return
	}
	// path: /{owner}/{repo}.git/<endpoint>
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	slash := strings.Index(trimmed, "/")
	if slash < 0 {
		writeErr(w, 404, "Not Found")
		return
	}
	owner := trimmed[:slash]
	rest := trimmed[slash+1:]
	dotGit := strings.Index(rest, ".git/")
	if dotGit < 0 {
		writeErr(w, 404, "Not Found")
		return
	}
	repoName := rest[:dotGit]
	endpoint := rest[dotGit+5:]
	repo := store.getRepo(owner, repoName)

	switch endpoint {
	case "info/refs":
		if r.Method != http.MethodGet {
			writeErr(w, 405, "method not allowed")
			return
		}
		service := r.URL.Query().Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			writeErr(w, 400, "missing or bad service")
			return
		}
		if repo == nil {
			writeErr(w, 404, "Not Found")
			return
		}
		w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		if service == "git-upload-pack" && strings.Contains(r.Header.Get("Git-Protocol"), "version=2") {
			w.Write(v2Advertisement())
			return
		}
		w.Write(v0Advertisement(repo, service))
	case "git-upload-pack":
		if r.Method != http.MethodPost {
			writeErr(w, 405, "method not allowed")
			return
		}
		if repo == nil {
			writeErr(w, 404, "Not Found")
			return
		}
		handleUploadPack(w, r, repo)
	case "git-receive-pack":
		if r.Method != http.MethodPost {
			writeErr(w, 405, "method not allowed")
			return
		}
		if repo == nil {
			writeErr(w, 404, "Not Found")
			return
		}
		handleReceivePack(w, r, repo)
	default:
		writeErr(w, 404, "Not Found")
	}
}

// ---- advertisements ----

func v2Advertisement() []byte {
	var buf bytes.Buffer
	buf.Write(pktText("version 2\n"))
	buf.Write(pktText("agent=forge/1.0\n"))
	buf.Write(pktText("ls-refs=unborn\n"))
	buf.Write(pktText("fetch=shallow\n"))
	buf.Write(pktText("object-format=sha1\n"))
	buf.Write(pktFlush)
	return buf.Bytes()
}

// refLines returns the advertisement ref entries: HEAD first (when the default
// branch is born), then every ref in name order, with peeled lines after
// annotated tags.
func advertisementRefs(repo *Repo) [][2]string {
	refs := repo.refSnapshot()
	var out [][2]string
	if tip, ok := refs["refs/heads/"+repo.DefaultBranch]; ok {
		out = append(out, [2]string{tip.Hex(), "HEAD"})
	}
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		o := refs[name]
		out = append(out, [2]string{o.Hex(), name})
		if obj := store.getObject(o); obj != nil && obj.Type == "tag" {
			if commit, ok := store.peelToCommit(o); ok {
				out = append(out, [2]string{commit.Hex(), name + "^{}"})
			}
		}
	}
	return out
}

func v0Advertisement(repo *Repo, service string) []byte {
	var caps string
	if service == "git-upload-pack" {
		caps = "multi_ack thin-pack side-band side-band-64k ofs-delta shallow symref=HEAD:refs/heads/" + repo.DefaultBranch + " agent=forge/1.0"
	} else {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0"
	}
	var buf bytes.Buffer
	buf.Write(pktText("# service=" + service + "\n"))
	buf.Write(pktFlush)
	refs := advertisementRefs(repo)
	if len(refs) == 0 {
		buf.Write(pktLine([]byte(zeroOID.Hex() + " capabilities^{}\x00" + caps)))
		return buf.Bytes()
	}
	for i, rl := range refs {
		line := rl[0] + " " + rl[1]
		if i == 0 {
			line += "\x00" + caps
		}
		buf.Write(pktLine([]byte(line)))
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

// ---- upload-pack ----

func handleUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 512<<20))
	if err != nil {
		writeErr(w, 400, "could not read body")
		return
	}
	pr := &pktReader{data: body}
	first, kind := pr.next()
	if pr.err != nil || kind != pktData {
		writeErr(w, 400, "malformed pkt-line stream")
		return
	}
	line := strings.TrimRight(string(first), "\n")
	if strings.HasPrefix(line, "command=") {
		command := strings.TrimPrefix(line, "command=")
		// skip capability lines up to the delimiter, then read arguments
		var args []string
		seenDelim := false
		for {
			payload, k := pr.next()
			if k == pktFlushKind {
				break
			}
			if k == pktDelimKind {
				seenDelim = true
				continue
			}
			if seenDelim {
				args = append(args, strings.TrimRight(string(payload), "\n"))
			}
		}
		if pr.err != nil {
			writeErr(w, 400, "malformed pkt-line stream")
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		switch command {
		case "ls-refs":
			w.Write(lsRefsResponse(repo, args))
		case "fetch":
			w.Write(fetchResponse(repo, args))
		default:
			writeErr(w, 400, "unknown command")
		}
		return
	}
	// v0 fetch fallback: first line is a want line; wants section ends at a
	// flush, negotiation section ends at a second flush (or EOF).
	wants := []oid{}
	haves := []oid{}
	clientShallow := []oid{}
	depth := 0
	parseNegotiation := func(l string) {
		switch {
		case strings.HasPrefix(l, "want "):
			// v0 want lines may carry capabilities after the oid
			fields := strings.Fields(l)
			if len(fields) >= 2 {
				if o, err := oidFromHex(fields[1]); err == nil {
					wants = append(wants, o)
				}
			}
		case strings.HasPrefix(l, "have "):
			if o, err := oidFromHex(strings.TrimSpace(l[5:])); err == nil {
				haves = append(haves, o)
			}
		case strings.HasPrefix(l, "shallow "):
			if o, err := oidFromHex(strings.TrimSpace(l[8:])); err == nil {
				clientShallow = append(clientShallow, o)
			}
		case strings.HasPrefix(l, "deepen "):
			depth, _ = strconv.Atoi(strings.TrimSpace(l[7:]))
		}
	}
	parseNegotiation(line)
	section := 0
	for {
		payload, k := pr.next()
		if k == pktFlushKind {
			section++
			if section >= 2 || pr.pos >= len(pr.data) {
				break
			}
			continue
		}
		if k == pktDelimKind {
			continue
		}
		parseNegotiation(strings.TrimRight(string(payload), "\n"))
	}
	plan := store.planFetch(wants, haves, clientShallow, depth, 0, nil)
	var buf bytes.Buffer
	buf.Write(pktText("NAK\n"))
	if len(plan.Objects) > 0 {
		pack := store.writePack(plan.Objects)
		writeSidebandPack(&buf, pack)
	}
	buf.Write(pktFlush)
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Write(buf.Bytes())
}

func lsRefsResponse(repo *Repo, args []string) []byte {
	peel := false
	symrefs := false
	unborn := false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(a, "ref-prefix "))
		}
	}
	match := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	refs := repo.refSnapshot()
	var buf bytes.Buffer
	defaultRef := "refs/heads/" + repo.DefaultBranch
	if tip, ok := refs[defaultRef]; ok {
		if match("HEAD") {
			line := tip.Hex() + " HEAD"
			if symrefs {
				line += " symref-target:" + defaultRef
			}
			buf.Write(pktText(line + "\n"))
		}
	} else if unborn && match("HEAD") {
		line := "unborn HEAD"
		if symrefs {
			line += " symref-target:" + defaultRef
		}
		buf.Write(pktText(line + "\n"))
	}
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if !match(name) {
			continue
		}
		o := refs[name]
		line := o.Hex() + " " + name
		if peel {
			if obj := store.getObject(o); obj != nil && obj.Type == "tag" {
				if commit, ok := store.peelToCommit(o); ok {
					line += " peeled:" + commit.Hex()
				}
			}
		}
		buf.Write(pktText(line + "\n"))
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

func fetchResponse(repo *Repo, args []string) []byte {
	var wants, haves, clientShallow, deepenNot []oid
	depth := 0
	deepenSince := int64(0)
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			if o, err := oidFromHex(strings.TrimSpace(a[5:])); err == nil {
				wants = append(wants, o)
			}
		case strings.HasPrefix(a, "have "):
			if o, err := oidFromHex(strings.TrimSpace(a[5:])); err == nil {
				haves = append(haves, o)
			}
		case strings.HasPrefix(a, "shallow "):
			if o, err := oidFromHex(strings.TrimSpace(a[8:])); err == nil {
				clientShallow = append(clientShallow, o)
			}
		case strings.HasPrefix(a, "deepen-since "):
			deepenSince, _ = strconv.ParseInt(strings.TrimSpace(a[13:]), 10, 64)
		case strings.HasPrefix(a, "deepen "):
			depth, _ = strconv.Atoi(strings.TrimSpace(a[7:]))
		case strings.HasPrefix(a, "deepen-not "):
			ref := strings.TrimSpace(a[11:])
			if o, ok := resolveRefToCommit(repo, strings.TrimPrefix(ref, "refs/heads/")); ok {
				deepenNot = append(deepenNot, o)
			} else if o, err := oidFromHex(ref); err == nil {
				deepenNot = append(deepenNot, o)
			}
		}
	}

	plan := store.planFetch(wants, haves, clientShallow, depth, deepenSince, deepenNot)

	var buf bytes.Buffer
	buf.Write(pktText("acknowledgments\n"))
	if plan.Empty {
		ack := ""
		for _, wn := range wants {
			for _, hv := range haves {
				if wn == hv {
					ack = wn.Hex()
				}
			}
		}
		if ack != "" {
			buf.Write(pktText("ACK " + ack + "\n"))
		} else if len(haves) > 0 {
			buf.Write(pktText("ACK " + haves[len(haves)-1].Hex() + "\n"))
		} else {
			buf.Write(pktText("NAK\n"))
		}
		buf.Write(pktFlush)
		return buf.Bytes()
	}
	common := ""
	for _, hv := range haves {
		if store.hasObject(hv) {
			common = hv.Hex()
		}
	}
	if common != "" {
		buf.Write(pktText("ACK " + common + "\n"))
	} else {
		buf.Write(pktText("NAK\n"))
	}
	buf.Write(pktDelim)
	if depth > 0 || deepenSince > 0 || len(clientShallow) > 0 {
		buf.Write(pktText("shallow-info\n"))
		for _, sc := range plan.Shallow {
			buf.Write(pktText("shallow " + sc.Hex() + "\n"))
		}
		for _, uc := range plan.Unshallow {
			buf.Write(pktText("unshallow " + uc.Hex() + "\n"))
		}
		buf.Write(pktDelim)
	}
	buf.Write(pktText("packfile\n"))
	pack := store.fetchPack(plan, wants, depth, deepenSince, clientShallow, haves)
	writeSidebandPack(&buf, pack)
	buf.Write(pktFlush)
	return buf.Bytes()
}

// fetchPack returns the pack bytes for a plan, using a cache for the hot
// repeated shape (depth-limited clone with no haves).
func (s *Store) fetchPack(plan fetchPlan, wants []oid, depth int, deepenSince int64, clientShallow, haves []oid) []byte {
	if len(haves) == 0 && deepenSince == 0 {
		var kb strings.Builder
		for _, wn := range wants {
			kb.WriteString(wn.Hex())
			kb.WriteByte(',')
		}
		kb.WriteString("|d=")
		kb.WriteString(strconv.Itoa(depth))
		for _, sc := range clientShallow {
			kb.WriteString("|s=")
			kb.WriteString(sc.Hex())
		}
		key := kb.String()
		s.packMu.Lock()
		cached, ok := s.packCache[key]
		s.packMu.Unlock()
		if ok {
			return cached
		}
		pack := s.writePack(plan.Objects)
		s.packMu.Lock()
		s.packCache[key] = pack
		s.packMu.Unlock()
		return pack
	}
	return s.writePack(plan.Objects)
}

// writeSidebandPack frames pack bytes as band-1 pkt-lines (<=65515 data bytes each).
func writeSidebandPack(buf *bytes.Buffer, pack []byte) {
	const chunk = 65515
	for len(pack) > 0 {
		n := chunk
		if len(pack) < n {
			n = len(pack)
		}
		payload := make([]byte, 0, n+1)
		payload = append(payload, 1)
		payload = append(payload, pack[:n]...)
		buf.Write(pktLine(payload))
		pack = pack[n:]
	}
}

// ---- receive-pack ----

type pushCommand struct {
	Old, New oid
	Ref      string
}

func handleReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 512<<20))
	if err != nil {
		writeErr(w, 400, "could not read body")
		return
	}
	pr := &pktReader{data: body}
	var commands []pushCommand
	for {
		payload, k := pr.next()
		if k == pktFlushKind {
			break
		}
		if k == pktDelimKind {
			continue
		}
		if pr.err != nil {
			break
		}
		line := string(payload)
		if i := strings.IndexByte(line, 0); i >= 0 {
			line = line[:i] // strip capabilities on the first line
		}
		line = strings.TrimRight(line, "\n")
		fields := strings.Fields(line)
		if len(fields) != 3 {
			pr.err = fmt.Errorf("bad ref update line")
			break
		}
		oldO, err1 := oidFromHex(fields[0])
		newO, err2 := oidFromHex(fields[1])
		if err1 != nil || err2 != nil || !strings.HasPrefix(fields[2], "refs/") {
			pr.err = fmt.Errorf("bad ref update values")
			break
		}
		commands = append(commands, pushCommand{Old: oldO, New: newO, Ref: fields[2]})
	}
	if pr.err != nil || len(commands) == 0 {
		// malformed push body: refuse plainly, stay alive
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(400)
		w.Write([]byte("malformed receive-pack body\n"))
		return
	}
	pack := pr.rest()
	unpackOK := true
	if len(pack) > 0 {
		if _, err := store.readPack(pack); err != nil {
			unpackOK = false
		}
	}

	type result struct {
		ref    string
		ok     bool
		reason string
	}
	results := make([]result, 0, len(commands))
	withRepoWrite(repo, func() {
		for _, cmd := range commands {
			current, exists := repo.Refs[cmd.Ref]
			if !exists {
				current = zeroOID
			}
			res := result{ref: cmd.Ref, ok: true}
			switch {
			case !unpackOK:
				res.ok, res.reason = false, "unpack failed"
			case cmd.Old != current:
				res.ok, res.reason = false, "stale info"
			case cmd.New.IsZero():
				// delete
				delete(repo.Refs, cmd.Ref)
			default:
				obj := store.getObject(cmd.New)
				if obj == nil {
					res.ok, res.reason = false, "unknown object"
					break
				}
				if !cmd.Old.IsZero() && obj.Type == "commit" {
					if !store.isAncestor(cmd.Old, cmd.New) {
						res.ok, res.reason = false, "non-fast-forward"
						break
					}
				}
				repo.Refs[cmd.Ref] = cmd.New
			}
			results = append(results, res)
		}
	})

	// report-status: a pkt-line stream carried inside band-1 packets
	var report bytes.Buffer
	if unpackOK {
		report.Write(pktText("unpack ok\n"))
	} else {
		report.Write(pktText("unpack error\n"))
	}
	for _, res := range results {
		if res.ok {
			report.Write(pktText("ok " + res.ref + "\n"))
		} else {
			report.Write(pktText("ng " + res.ref + " " + res.reason + "\n"))
		}
	}
	report.Write(pktFlush)

	var out bytes.Buffer
	writeSidebandPack(&out, report.Bytes())
	out.Write(pktFlush)
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Write(out.Bytes())
}
