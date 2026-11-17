package main

// Git object model: SHA-1 addressed objects, pack read/write, delta application,
// tree/commit parsing and construction. Standard library only.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

type oid [20]byte

var zeroOID oid

func oidFromHex(s string) (oid, error) {
	var o oid
	if len(s) != 40 {
		return o, fmt.Errorf("bad oid length %d", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return o, err
	}
	copy(o[:], b)
	return o, nil
}

func (o oid) Hex() string  { return hex.EncodeToString(o[:]) }
func (o oid) IsZero() bool { return o == zeroOID }

func hashObject(typ string, data []byte) oid {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	var o oid
	copy(o[:], h.Sum(nil))
	return o
}

// gitObject is one stored object: its type and its raw payload (no header).
type gitObject struct {
	Type string
	Data []byte
	Z    []byte // zlib-compressed payload, computed at insert time for pack writing
}

func zlibCompress(data []byte) []byte {
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, zlib.DefaultCompression)
	zw.Write(data)
	zw.Close()
	return buf.Bytes()
}

func zlibInflate(data []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// ---- commits ----

type commitInfo struct {
	Tree        oid
	Parents     []oid
	Author      string // full "Name <email> ts tz" line value
	Commit      string
	CommitterTs int64
	Message     string
}

func parseCommit(data []byte) (*commitInfo, error) {
	ci := &commitInfo{}
	rest := data
	for {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return nil, errors.New("commit: unterminated header")
		}
		line := rest[:nl]
		rest = rest[nl+1:]
		if len(line) == 0 {
			break
		}
		// continuation lines of multi-line headers (gpgsig) start with a space; skip them
		if line[0] == ' ' {
			continue
		}
		key, val, found := strings.Cut(string(line), " ")
		if !found {
			continue
		}
		switch key {
		case "tree":
			t, err := oidFromHex(val)
			if err != nil {
				return nil, err
			}
			ci.Tree = t
		case "parent":
			p, err := oidFromHex(val)
			if err != nil {
				return nil, err
			}
			ci.Parents = append(ci.Parents, p)
		case "author":
			ci.Author = val
		case "committer":
			ci.Commit = val
			ci.CommitterTs = identTimestamp(val)
		}
	}
	ci.Message = string(rest)
	return ci, nil
}

// identTimestamp extracts the unix timestamp from a "Name <email> 1700000000 +0000" ident.
func identTimestamp(ident string) int64 {
	// timestamp is the second-to-last whitespace-separated field
	fields := strings.Fields(ident)
	for i := len(fields) - 1; i >= 0; i-- {
		if ts, err := strconv.ParseInt(fields[i], 10, 64); err == nil && ts > 100000000 {
			return ts
		}
	}
	return 0
}

// identParts splits "Name <email> ts tz" into name, email, ts.
func identParts(ident string) (name, email string, ts int64) {
	lt := strings.Index(ident, "<")
	gt := strings.Index(ident, ">")
	if lt < 0 || gt < 0 || gt < lt {
		return strings.TrimSpace(ident), "", identTimestamp(ident)
	}
	return strings.TrimSpace(ident[:lt]), ident[lt+1 : gt], identTimestamp(ident)
}

func buildCommit(tree oid, parents []oid, author, committer, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree.Hex() + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p.Hex() + "\n")
	}
	b.WriteString("author " + author + "\n")
	b.WriteString("committer " + committer + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// ---- trees ----

type treeEntry struct {
	Mode string // e.g. "100644", "40000" (stored form, no leading zero)
	Name string
	Oid  oid
}

func (e treeEntry) IsDir() bool { return e.Mode == "40000" }

func parseTree(data []byte) ([]treeEntry, error) {
	var out []treeEntry
	rest := data
	for len(rest) > 0 {
		sp := bytes.IndexByte(rest, ' ')
		if sp < 0 {
			return nil, errors.New("tree: bad entry")
		}
		mode := string(rest[:sp])
		rest = rest[sp+1:]
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 || len(rest) < nul+21 {
			return nil, errors.New("tree: truncated entry")
		}
		name := string(rest[:nul])
		var o oid
		copy(o[:], rest[nul+1:nul+21])
		rest = rest[nul+21:]
		out = append(out, treeEntry{Mode: mode, Name: name, Oid: o})
	}
	return out, nil
}

// gitTreeSortKey is the comparison key git uses: name with '/' appended for directories.
func gitTreeSortKey(e treeEntry) string {
	if e.IsDir() {
		return e.Name + "/"
	}
	return e.Name
}

func buildTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return gitTreeSortKey(sorted[i]) < gitTreeSortKey(sorted[j]) })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		buf.Write(e.Oid[:])
	}
	return buf.Bytes()
}

// ---- annotated tags ----

func parseTagTarget(data []byte) (oid, string, error) {
	// "object <hex>\ntype <t>\n..."
	lines := strings.SplitN(string(data), "\n", 4)
	var target oid
	var typ string
	for _, ln := range lines {
		if strings.HasPrefix(ln, "object ") {
			o, err := oidFromHex(strings.TrimSpace(ln[7:]))
			if err != nil {
				return target, "", err
			}
			target = o
		}
		if strings.HasPrefix(ln, "type ") {
			typ = strings.TrimSpace(ln[5:])
		}
	}
	if target.IsZero() {
		return target, "", errors.New("tag: no object line")
	}
	return target, typ, nil
}

// peelToCommit resolves an object to the commit it ultimately points at (through tags).
func (s *Store) peelToCommit(o oid) (oid, bool) {
	for depth := 0; depth < 16; depth++ {
		obj := s.getObject(o)
		if obj == nil {
			return o, false
		}
		switch obj.Type {
		case "commit":
			return o, true
		case "tag":
			next, _, err := parseTagTarget(obj.Data)
			if err != nil {
				return o, false
			}
			o = next
		default:
			return o, false
		}
	}
	return o, false
}

// ---- packs ----

var packTypeNames = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag", 6: "ofs-delta", 7: "ref-delta"}
var packTypeNums = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}

// writePack serialises the given objects (already in the store) as a complete,
// non-thin packfile, version 2, with a correct SHA-1 trailer.
func (s *Store) writePack(objs []oid) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	writeU32(&buf, 2)
	writeU32(&buf, uint32(len(objs)))
	for _, o := range objs {
		obj := s.getObject(o)
		if obj == nil {
			continue
		}
		writePackEntryHeader(&buf, packTypeNums[obj.Type], len(obj.Data))
		buf.Write(obj.Z)
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func writeU32(buf *bytes.Buffer, v uint32) {
	buf.WriteByte(byte(v >> 24))
	buf.WriteByte(byte(v >> 16))
	buf.WriteByte(byte(v >> 8))
	buf.WriteByte(byte(v))
}

func writePackEntryHeader(buf *bytes.Buffer, typ int, size int) {
	// varint: first byte holds 4 size bits + 3 type bits + continuation
	b := byte((size & 0x0f) | (typ << 4))
	size >>= 4
	for size > 0 {
		buf.WriteByte(b | 0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	buf.WriteByte(b)
}

// readPack parses a packfile and inserts every object into the store.
// Returns the number of objects inserted.
func (s *Store) readPack(pack []byte) (int, error) {
	if len(pack) < 12+20 {
		return 0, errors.New("pack: too short")
	}
	if string(pack[:4]) != "PACK" {
		return 0, errors.New("pack: bad signature")
	}
	version := uint32(pack[4])<<24 | uint32(pack[5])<<16 | uint32(pack[6])<<8 | uint32(pack[7])
	if version != 2 {
		return 0, fmt.Errorf("pack: unsupported version %d", version)
	}
	count := int(uint32(pack[8])<<24 | uint32(pack[9])<<16 | uint32(pack[10])<<8 | uint32(pack[11]))
	// verify trailer
	want := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(want[:], pack[len(pack)-20:]) {
		return 0, errors.New("pack: checksum mismatch")
	}
	pos := 12
	type pending struct {
		typ  string
		data []byte
	}
	var inserted []pending
	offsets := map[int]oid{} // pack offset -> oid, for ofs-delta
	for i := 0; i < count; i++ {
		start := pos
		if pos >= len(pack)-20 {
			return 0, errors.New("pack: truncated entry header")
		}
		c := pack[pos]
		pos++
		typNum := int(c>>4) & 0x7
		size := int64(c & 0x0f)
		shift := uint(4)
		for c&0x80 != 0 {
			if pos >= len(pack) {
				return 0, errors.New("pack: truncated size")
			}
			c = pack[pos]
			pos++
			size |= int64(c&0x7f) << shift
			shift += 7
		}
		typ := packTypeNames[typNum]
		var baseOid oid
		var baseFromOffset int
		switch typNum {
		case 6: // ofs-delta: negative offset varint
			if pos >= len(pack) {
				return 0, errors.New("pack: truncated ofs-delta")
			}
			c = pack[pos]
			pos++
			off := int64(c & 0x7f)
			for c&0x80 != 0 {
				c = pack[pos]
				pos++
				off = ((off + 1) << 7) | int64(c&0x7f)
			}
			baseFromOffset = start - int(off)
		case 7: // ref-delta: 20-byte base oid
			if pos+20 > len(pack) {
				return 0, errors.New("pack: truncated ref-delta")
			}
			copy(baseOid[:], pack[pos:pos+20])
			pos += 20
		}
		// zlib stream: inflate through a byte-counting ByteReader so the exact
		// compressed length is known afterwards (no buffered over-read).
		cr := &countReader{r: bytes.NewReader(pack[pos:])}
		zr, err := zlib.NewReader(cr)
		if err != nil {
			return 0, fmt.Errorf("pack: zlib: %w", err)
		}
		data, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return 0, fmt.Errorf("pack: inflate: %w", err)
		}
		if int64(len(data)) != size {
			return 0, fmt.Errorf("pack: entry size mismatch: header %d, inflated %d", size, len(data))
		}
		pos += int(cr.n)
		var objData []byte
		var objType string
		switch typNum {
		case 6, 7:
			var base *gitObject
			if typNum == 6 {
				bo, ok := offsets[baseFromOffset]
				if !ok {
					return 0, errors.New("pack: ofs-delta base not in pack")
				}
				base = s.getObject(bo)
			} else {
				base = s.getObject(baseOid)
			}
			if base == nil {
				return 0, errors.New("pack: delta base missing (thin pack not supported)")
			}
			objData, err = applyDelta(base.Data, data)
			if err != nil {
				return 0, err
			}
			objType = base.Type
		default:
			objData = data
			objType = typ
		}
		id := s.putObject(objType, objData)
		offsets[start] = id
		inserted = append(inserted, pending{objType, objData})
	}
	return len(inserted), nil
}

// countReader counts the bytes consumed from the underlying reader. It
// implements io.ByteReader so compress/zlib reads through it directly,
// byte-exact, without a buffering layer that would over-read into the next
// pack entry.
type countReader struct {
	r *bytes.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// applyDelta applies a git delta (copy/insert program) to a base payload.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int64, error) {
		var v int64
		var shift uint
		for {
			if pos >= len(delta) {
				return 0, errors.New("delta: truncated varint")
			}
			c := delta[pos]
			pos++
			v |= int64(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != int64(len(base)) {
		return nil, fmt.Errorf("delta: base size %d, delta expects %d", len(base), srcSize)
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			var off, sz int64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta: truncated copy")
					}
					off |= int64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta: truncated copy")
					}
					sz |= int64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if sz == 0 {
				sz = 0x10000
			}
			if off+sz > int64(len(base)) {
				return nil, errors.New("delta: copy out of range")
			}
			out = append(out, base[off:off+sz]...)
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta: truncated insert")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, errors.New("delta: opcode 0 is reserved")
		}
	}
	if int64(len(out)) != dstSize {
		return nil, fmt.Errorf("delta: result size %d, expected %d", len(out), dstSize)
	}
	return out, nil
}
