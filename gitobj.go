// Git object model: SHA-1 ids, pkt-line framing, packfile read/write and
// graph walks. Port of entry/gitobj.ts (TypeScript) to Go, behaviour kept
// identical: same hashes, same error conditions, same traversal order.
package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

const ZeroOID = "0000000000000000000000000000000000000000"

type GitObjectType string

const (
	ObjCommit GitObjectType = "commit"
	ObjTree   GitObjectType = "tree"
	ObjBlob   GitObjectType = "blob"
	ObjTag    GitObjectType = "tag"
)

type GitObject struct {
	Type GitObjectType
	Data []byte
}

type ObjectLookup func(oid string) *GitObject

var typeCode = map[GitObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]GitObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func hashObject(typ GitObjectType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func isObjectId(v string) bool {
	if len(v) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// pkt-line framing
// ---------------------------------------------------------------------------

func pktLine(payload []byte) []byte {
	l := len(payload) + 4
	out := make([]byte, 0, l)
	out = append(out, []byte(fmt.Sprintf("%04x", l))...)
	out = append(out, payload...)
	return out
}

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }

type DecodedPacket struct {
	Kind    string // "data" | "flush" | "delim"
	Payload []byte
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// Decode pkt-lines from the front of buf. Returns packets and the offset
// where the stream stopped (byte after terminating flush, or first byte
// that is not a pkt-line).
func decodePktLines(buf []byte, maxPackets ...int) ([]DecodedPacket, int, error) {
	max := 100000
	if len(maxPackets) > 0 {
		max = maxPackets[0]
	}
	var packets []DecodedPacket
	offset := 0
	for offset+4 <= len(buf) {
		if !isHexByte(buf[offset]) || !isHexByte(buf[offset+1]) || !isHexByte(buf[offset+2]) || !isHexByte(buf[offset+3]) {
			return nil, 0, fmt.Errorf("bad pkt-line length %q at offset %d", string(buf[offset:offset+4]), offset)
		}
		var length int
		fmt.Sscanf(string(buf[offset:offset+4]), "%04x", &length)
		// Sscanf with %x is lenient; parse strictly instead.
		length = hexVal(buf[offset])<<12 | hexVal(buf[offset+1])<<8 | hexVal(buf[offset+2])<<4 | hexVal(buf[offset+3])
		if length == 0 {
			packets = append(packets, DecodedPacket{Kind: "flush"})
			return packets, offset + 4, nil
		}
		if length == 1 {
			packets = append(packets, DecodedPacket{Kind: "delim"})
			offset += 4
			continue
		}
		if length < 4 || offset+length > len(buf) {
			return nil, 0, fmt.Errorf("pkt-line length %d overruns the body at offset %d", length, offset)
		}
		payload := make([]byte, length-4)
		copy(payload, buf[offset+4:offset+length])
		packets = append(packets, DecodedPacket{Kind: "data", Payload: payload})
		offset += length
		if len(packets) >= max {
			return nil, 0, errors.New("too many pkt-lines")
		}
	}
	return packets, offset, nil
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// Encode text lines (each ending in \n) plus trailing flush/delim.
func encodePktLines(lines []string) []byte {
	var out []byte
	for _, line := range lines {
		if line == "flush" {
			out = append(out, pktFlush()...)
		} else if line == "delim" {
			out = append(out, pktDelim()...)
		} else {
			out = append(out, pktLine([]byte(line))...)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// trees and commits
// ---------------------------------------------------------------------------

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], ' ')
		if space < 0 {
			return nil, errors.New("tree entry has no mode")
		}
		space += offset
		mode := string(data[offset:space])
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry has no name/oid")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry has no name/oid")
		}
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		offset = nul + 21
	}
	return entries, nil
}

func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func sortTreeEntries(entries []TreeEntry) []TreeEntry {
	out := make([]TreeEntry, len(entries))
	copy(out, entries)
	sort.SliceStable(out, func(i, j int) bool { return treeSortKey(out[i]) < treeSortKey(out[j]) })
	return out
}

func buildTree(entries []TreeEntry) []byte {
	var buf bytes.Buffer
	for _, e := range sortTreeEntries(entries) {
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		b, _ := hex.DecodeString(e.OID)
		buf.Write(b)
	}
	return buf.Bytes()
}

type CommitView struct {
	Tree        string
	Parents     []string
	Author      string
	AuthorTs    int64
	Committer   string
	CommitterTs int64
	Message     string
}

var authorRe = regexp.MustCompile(`^(.*) (\d+) [+-]\d{4}$`)

func parseCommit(data []byte) CommitView {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head, message := text, ""
	if split >= 0 {
		head = text[:split]
		message = text[split+2:]
	}
	v := CommitView{}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			v.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			v.Parents = append(v.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			rest := line[7:]
			if m := authorRe.FindStringSubmatch(rest); m != nil {
				v.Author = m[1]
				fmt.Sscanf(m[2], "%d", &v.AuthorTs)
			} else {
				v.Author = rest
			}
		case strings.HasPrefix(line, "committer "):
			rest := line[10:]
			if m := authorRe.FindStringSubmatch(rest); m != nil {
				v.Committer = m[1]
				fmt.Sscanf(m[2], "%d", &v.CommitterTs)
			} else {
				v.Committer = rest
			}
		}
	}
	v.Message = message
	return v
}

func buildCommit(tree string, parents []string, author, committer string, timestamp int64, message string) []byte {
	var lines []string
	lines = append(lines, "tree "+tree)
	for _, p := range parents {
		lines = append(lines, "parent "+p)
	}
	lines = append(lines, fmt.Sprintf("author %s %d +0000", author, timestamp))
	lines = append(lines, fmt.Sprintf("committer %s %d +0000", committer, timestamp))
	msg := message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	return []byte(strings.Join(lines, "\n") + "\n\n" + msg)
}

type TagView struct {
	Object   string
	Type     string
	Tag      string
	Tagger   string
	TaggerTs int64
	Message  string
}

func parseTag(data []byte) TagView {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head, message := text, ""
	if split >= 0 {
		head = text[:split]
		message = text[split+2:]
	}
	v := TagView{}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			v.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			v.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			v.Tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			rest := line[7:]
			if m := authorRe.FindStringSubmatch(rest); m != nil {
				v.Tagger = m[1]
				fmt.Sscanf(m[2], "%d", &v.TaggerTs)
			} else {
				v.Tagger = rest
			}
		}
	}
	v.Message = message
	return v
}

func buildTag(object, typ, tag, tagger string, timestamp int64, message string) []byte {
	msg := message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	return []byte(fmt.Sprintf("object %s\ntype %s\ntag %s\ntagger %s %d +0000\n\n%s", object, typ, tag, tagger, timestamp, msg))
}

// ---------------------------------------------------------------------------
// deltas and packfiles
// ---------------------------------------------------------------------------

func readVarintSize(buf []byte, start int) (size int, next int, first byte, err error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("pack entry size varint is truncated")
	}
	b := buf[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, 0, errors.New("pack entry size varint is truncated")
		}
		b = buf[offset]
		offset++
		size += int(b&0x7f) << shift
		shift += 7
		if shift > 63 {
			return 0, 0, 0, errors.New("pack entry size varint is too long")
		}
	}
	return size, offset, first, nil
}

func readOfsDeltaBase(buf []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("ofs-delta base offset is truncated")
	}
	b := buf[start]
	offset := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, errors.New("ofs-delta base offset is truncated")
		}
		b = buf[offset]
		offset++
		value = (value+1)*128 + int(b&0x7f)
	}
	return value, offset, nil
}

// Inflate one zlib stream starting at offset; returns bytes and offset past it.
// Go's zlib reader over a *bytes.Reader consumes exactly the stream bytes
// (verified by probe: reader position lands on the next stream).
func inflateAt(buf []byte, offset int) ([]byte, int, error) {
	r := bytes.NewReader(buf[offset:])
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	if len(data) == 0 {
		// An empty object is legal in principle; TS only errors when the
		// engine reports 0 bytes written for a non-empty stream. Keep the
		// same guard via the declared-size check in readPack.
	}
	consumed := len(buf) - offset - r.Len()
	return data, offset + consumed, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := delta[offset]
			offset++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
			if shift > 63 {
				return 0, errors.New("delta size varint is too long")
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy offset is truncated")
					}
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy size is truncated")
					}
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) {
				return nil, errors.New("delta copy is out of the base")
			}
			if written+copySize > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if offset+literal > len(delta) {
				return nil, errors.New("delta insert is truncated")
			}
			if written+literal > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:written+literal], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

type rawEntry struct {
	offset       int
	typeCode     int
	size         int
	data         []byte
	baseOffset   int
	baseOid      string
	resolvedType GitObjectType
	resolved     []byte
	oid          string
	hasResolved  bool
}

func readPack(pack []byte, resolveBase ObjectLookup) (map[string]*GitObject, error) {
	buf := pack
	if len(buf) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(buf))
	}
	if string(buf[0:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := int(buf[4])<<24 | int(buf[5])<<16 | int(buf[6])<<8 | int(buf[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(buf[8])<<24 | int(buf[9])<<16 | int(buf[10])<<8 | int(buf[11])
	trailerStart := len(buf) - 20
	trailer := hex.EncodeToString(buf[trailerStart:])
	h := sha1.New()
	h.Write(buf[:trailerStart])
	computed := hex.EncodeToString(h.Sum(nil))
	if trailer != computed {
		return nil, errors.New("pack trailer checksum does not match its bytes")
	}
	raw := make([]*rawEntry, 0, declaredCount)
	byOffset := map[int]*rawEntry{}
	offset := 12
	for index := 0; index < declaredCount; index++ {
		entryOffset := offset
		size, next, first, err := readVarintSize(buf, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		tc := int(first>>4) & 0x07
		entry := &rawEntry{offset: entryOffset, typeCode: tc, size: size}
		if tc == 6 {
			base, nxt, err := readOfsDeltaBase(buf, offset)
			if err != nil {
				return nil, err
			}
			offset = nxt
			if base <= 0 || base > entryOffset {
				return nil, fmt.Errorf("ofs-delta at %d names an invalid base offset %d", entryOffset, base)
			}
			entry.baseOffset = entryOffset - base
		} else if tc == 7 {
			if offset+20 > trailerStart {
				return nil, errors.New("ref-delta base id is truncated")
			}
			entry.baseOid = hex.EncodeToString(buf[offset : offset+20])
			offset += 20
		} else if _, ok := codeType[tc]; !ok {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", index, tc)
		}
		inflated, nxt, err := inflateAt(buf[:trailerStart], offset)
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", entryOffset, size, len(inflated))
		}
		entry.data = inflated
		offset = nxt
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-offset)
	}
	objects := map[string]*GitObject{}
	resolvedByOid := map[string]*rawEntry{}
	resolving := map[int]bool{}
	var resolve func(entry *rawEntry) (GitObjectType, []byte, string, error)
	resolve = func(entry *rawEntry) (GitObjectType, []byte, string, error) {
		if entry.hasResolved {
			return entry.resolvedType, entry.resolved, entry.oid, nil
		}
		if resolving[entry.offset] {
			return "", nil, "", fmt.Errorf("delta cycle at pack offset %d", entry.offset)
		}
		resolving[entry.offset] = true
		var typ GitObjectType
		var data []byte
		var err error
		if entry.typeCode == 6 {
			baseEntry := byOffset[entry.baseOffset]
			if baseEntry == nil {
				delete(resolving, entry.offset)
				return "", nil, "", fmt.Errorf("ofs-delta base at offset %d is not in the pack", entry.baseOffset)
			}
			var bt GitObjectType
			var bd []byte
			bt, bd, _, err = resolve(baseEntry)
			if err != nil {
				delete(resolving, entry.offset)
				return "", nil, "", err
			}
			typ = bt
			data, err = applyDelta(bd, entry.data)
			if err != nil {
				delete(resolving, entry.offset)
				return "", nil, "", err
			}
		} else if entry.typeCode == 7 {
			wanted := entry.baseOid
			if baseEntry, ok := resolvedByOid[wanted]; ok {
				var bt GitObjectType
				var bd []byte
				bt, bd, _, err = resolve(baseEntry)
				if err != nil {
					delete(resolving, entry.offset)
					return "", nil, "", err
				}
				typ = bt
				data, err = applyDelta(bd, entry.data)
				if err != nil {
					delete(resolving, entry.offset)
					return "", nil, "", err
				}
			} else {
				var external *GitObject
				if resolveBase != nil {
					external = resolveBase(wanted)
				}
				if external == nil {
					delete(resolving, entry.offset)
					return "", nil, "", fmt.Errorf("ref-delta base %s is neither in the pack nor on the server", wanted)
				}
				typ = external.Type
				data, err = applyDelta(external.Data, entry.data)
				if err != nil {
					delete(resolving, entry.offset)
					return "", nil, "", err
				}
			}
		} else {
			typ = codeType[entry.typeCode]
			data = entry.data
		}
		oid := hashObject(typ, data)
		entry.resolved = data
		entry.resolvedType = typ
		entry.oid = oid
		entry.hasResolved = true
		resolvedByOid[oid] = entry
		delete(resolving, entry.offset)
		return typ, data, oid, nil
	}
	for _, entry := range raw {
		typ, data, oid, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = &GitObject{Type: typ, Data: data}
		}
	}
	return objects, nil
}

func writePackObjectHeader(typ GitObjectType, size int) []byte {
	code := typeCode[typ]
	var bytesOut []byte
	remaining := size
	first := byte((code << 4) | (remaining & 0x0f))
	remaining = remaining / 16
	for remaining > 0 {
		bytesOut = append(bytesOut, first|0x80)
		first = byte(remaining & 0x7f)
		remaining = remaining / 128
	}
	bytesOut = append(bytesOut, first)
	return bytesOut
}

// Build a packfile from plain objects (no deltas), in caller order.
// Deflate level 1, like the TS entry.
func writePack(objects []*GitObject) []byte {
	var body bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(objects)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	body.Write(header)
	for _, obj := range objects {
		body.Write(writePackObjectHeader(obj.Type, len(obj.Data)))
		var zb bytes.Buffer
		w, _ := zlib.NewWriterLevel(&zb, 1)
		w.Write(obj.Data)
		w.Close()
		body.Write(zb.Bytes())
	}
	raw := body.Bytes()
	h := sha1.New()
	h.Write(raw)
	return append(raw, h.Sum(nil)...)
}

// Ensure flate import is used (deflate level is via zlib above; keep for
// future single-inflate paths).
var _ = flate.NewReader

// ---------------------------------------------------------------------------
// graph walks
// ---------------------------------------------------------------------------

func peelObject(lookup ObjectLookup, oid string) (GitObjectType, string) {
	current := oid
	for hops := 0; hops < 16; hops++ {
		obj := lookup(current)
		if obj == nil || obj.Type != ObjTag {
			if obj != nil {
				return obj.Type, current
			}
			return ObjCommit, current
		}
		current = parseTag(obj.Data).Object
	}
	return ObjCommit, current
}

type ClosureResult struct {
	Objects    map[string]bool
	CommitDist map[string]int
	Boundary   []string
}

func collectClosure(lookup ObjectLookup, tips []string, depthLimit *int, stopAt map[string]bool) ClosureResult {
	objects := map[string]bool{}
	commitDist := map[string]int{}
	type item struct {
		oid  string
		dist int
	}
	var queue []item
	seenCommit := map[string]bool{}
	for _, tip := range tips {
		peeledType, peeledOid := peelObject(lookup, tip)
		obj := lookup(tip)
		if obj != nil && obj.Type == ObjTag {
			objects[tip] = true
		}
		if peeledType == ObjCommit {
			if !seenCommit[peeledOid] {
				seenCommit[peeledOid] = true
				commitDist[peeledOid] = 0
				queue = append(queue, item{peeledOid, 0})
			}
		} else {
			if o := lookup(peeledOid); o != nil {
				objects[peeledOid] = true
			}
		}
	}
	var boundary []string
	head := 0
	for head < len(queue) {
		it := queue[head]
		head++
		commitObj := lookup(it.oid)
		if commitObj == nil || commitObj.Type != ObjCommit {
			continue
		}
		objects[it.oid] = true
		view := parseCommit(commitObj.Data)
		if treeObj := lookup(view.Tree); treeObj != nil {
			objects[view.Tree] = true
			var walkTree func(treeOid string)
			walkTree = func(treeOid string) {
				t := lookup(treeOid)
				if t == nil || t.Type != ObjTree {
					return
				}
				entries, err := parseTree(t.Data)
				if err != nil {
					return
				}
				for _, e := range entries {
					objects[e.OID] = true
					if e.Mode == "40000" {
						walkTree(e.OID)
					}
				}
			}
			walkTree(view.Tree)
		}
		limited := depthLimit != nil && it.dist+1 >= *depthLimit
		stopped := stopAt != nil && stopAt[it.oid]
		if limited || stopped {
			if len(view.Parents) > 0 {
				boundary = append(boundary, it.oid)
			}
			continue
		}
		for _, parent := range view.Parents {
			if !seenCommit[parent] {
				seenCommit[parent] = true
				commitDist[parent] = it.dist + 1
				queue = append(queue, item{parent, it.dist + 1})
			}
		}
	}
	return ClosureResult{Objects: objects, CommitDist: commitDist, Boundary: boundary}
}

func isAncestor(lookup ObjectLookup, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	queue := []string{descendant}
	head := 0
	for head < len(queue) {
		oid := queue[head]
		head++
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		if obj.Type == ObjTag {
			queue = append(queue, parseTag(obj.Data).Object)
			continue
		}
		if obj.Type != ObjCommit {
			continue
		}
		for _, parent := range parseCommit(obj.Data).Parents {
			if parent == ancestor {
				return true
			}
			queue = append(queue, parent)
		}
	}
	return false
}

func mergeBase(lookup ObjectLookup, a, b string) string {
	ancestorsA := map[string]bool{}
	_, aOid := peelObject(lookup, a)
	queueA := []string{aOid}
	headA := 0
	for headA < len(queueA) {
		oid := queueA[headA]
		headA++
		if ancestorsA[oid] {
			continue
		}
		ancestorsA[oid] = true
		if obj := lookup(oid); obj != nil && obj.Type == ObjCommit {
			queueA = append(queueA, parseCommit(obj.Data).Parents...)
		}
	}
	seen := map[string]bool{}
	_, bOid := peelObject(lookup, b)
	queueB := []string{bOid}
	headB := 0
	for headB < len(queueB) {
		oid := queueB[headB]
		headB++
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancestorsA[oid] {
			return oid
		}
		if obj := lookup(oid); obj != nil && obj.Type == ObjCommit {
			queueB = append(queueB, parseCommit(obj.Data).Parents...)
		}
	}
	return ""
}

func flattenTree(lookup ObjectLookup, treeOid, prefix string) map[string]struct {
	OID  string
	Mode string
} {
	out := map[string]struct {
		OID  string
		Mode string
	}{}
	var walk func(oid, dir string)
	walk = func(oid, dir string) {
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjTree {
			return
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return
		}
		for _, e := range entries {
			path := e.Name
			if dir != "" {
				path = dir + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, path)
			} else {
				out[path] = struct {
					OID  string
					Mode string
				}{e.OID, e.Mode}
			}
		}
	}
	walk(treeOid, prefix)
	return out
}

func listCommits(lookup ObjectLookup, tip string) []string {
	var out []string
	seen := map[string]bool{}
	_, start := peelObject(lookup, tip)
	queue := []string{start}
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
		out = append(out, oid)
		queue = append(queue, parseCommit(obj.Data).Parents...)
	}
	return out
}
