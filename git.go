// Git smart HTTP: info/refs advertisements, protocol v2 upload-pack
// (ls-refs + fetch, shallow/deepen), protocol v0 fallback, receive-pack.
// Port of entry/git.ts.
package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

type GitRequest struct {
	Method  string
	Path    string
	Query   map[string][]string
	Headers map[string]string
	Body    []byte
}

func (r *GitRequest) QueryGet(k string) string {
	if v, ok := r.Query[k]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

type GitResponse struct {
	Status       int
	ContentType  string
	Body         []byte
	ExtraHeaders map[string]string
}

const (
	uploadAd      = "application/x-git-upload-pack-advertisement"
	uploadResult  = "application/x-git-upload-pack-result"
	receiveAd     = "application/x-git-receive-pack-advertisement"
	receiveResult = "application/x-git-receive-pack-result"
)

func sortedRefs(repo *Repo) []struct {
	Name string
	OID  string
} {
	out := make([]struct {
		Name string
		OID  string
	}, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		out = append(out, struct {
			Name string
			OID  string
		}{name, oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func peelToCommit(repo *Repo, oid string) string {
	typ, peeled := peelObject(func(id string) *GitObject { return repo.Objects[id] }, oid)
	if typ == ObjCommit {
		return peeled
	}
	return ""
}

func headOid(repo *Repo) string {
	if oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
		return oid
	}
	return ZeroOID
}

var v2AdvertisementBytes = encodePktLines([]string{
	"version 2\n",
	"agent=makefun-forge/1.0\n",
	"ls-refs=unborn\n",
	"fetch=shallow\n",
	"object-format=sha1\n",
	"flush",
})

func pktLineStr(s string) string {
	l := len(s) + 4
	return fmt.Sprintf("%04x%s", l, s)
}

func v0Advertisement(repo *Repo, service string) []byte {
	var caps string
	if service == "git-upload-pack" {
		caps = "side-band-64k thin-pack ofs-delta shallow multi_ack multi_ack_detailed symref=HEAD:refs/heads/main agent=makefun-forge/1.0"
	} else {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=makefun-forge/1.0"
	}
	out := pktLineStr("# service="+service+"\n") + "0000"
	refs := sortedRefs(repo)
	if len(refs) == 0 {
		out += pktLineStr(ZeroOID + " capabilities^{}\x00" + caps + "\n")
	} else {
		head := headOid(repo)
		out += pktLineStr(head + " HEAD\x00" + caps + "\n")
		for _, ref := range refs {
			out += pktLineStr(ref.OID + " " + ref.Name + "\n")
			if obj := repo.Objects[ref.OID]; obj != nil && obj.Type == ObjTag {
				if peeled := peelToCommit(repo, ref.OID); peeled != "" {
					out += pktLineStr(peeled + " " + ref.Name + "^{}\n")
				}
			}
		}
	}
	out += "0000"
	return []byte(out)
}

func infoRefs(repo *Repo, service, protocol string) GitResponse {
	if service == "git-upload-pack" && protocol == "version=2" {
		return GitResponse{Status: 200, ContentType: uploadAd, Body: v2AdvertisementBytes}
	}
	ct := uploadAd
	if service != "git-upload-pack" {
		ct = receiveAd
	}
	return GitResponse{Status: 200, ContentType: ct, Body: v0Advertisement(repo, service)}
}

func lsRefsV2(repo *Repo, peel, symrefs, unborn bool, prefixes []string) []byte {
	var out strings.Builder
	head := headOid(repo)
	keep := func(name string) bool {
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
	if keep("HEAD") {
		if head == ZeroOID {
			if unborn {
				out.WriteString(pktLineStr("unborn HEAD symref-target:refs/heads/main\n"))
			}
		} else {
			out.WriteString(pktLineStr(head + " HEAD symref-target:refs/heads/main\n"))
		}
	}
	for _, ref := range sortedRefs(repo) {
		if !keep(ref.Name) {
			continue
		}
		line := ref.OID + " " + ref.Name
		if peel {
			if obj := repo.Objects[ref.OID]; obj != nil && obj.Type == ObjTag {
				if peeled := peelToCommit(repo, ref.OID); peeled != "" {
					line += " peeled:" + peeled
				}
			}
		}
		out.WriteString(pktLineStr(line + "\n"))
	}
	out.WriteString("0000")
	return []byte(out.String())
}

// ---------------------------------------------------------------------------
// fetch

type FetchPlan struct {
	Wants             []string
	Haves             []string
	ShallowBoundaries []string
	Depth             *int
}

type FetchSelection struct {
	Send      []*GitObject
	SendOids  map[string]bool
	Shallow   []string
	Unshallow []string
	Acks      []string
	HasCommon bool
}

func selectFetch(repo *Repo, plan FetchPlan) FetchSelection {
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	stopAt := map[string]bool{}
	for _, s := range plan.ShallowBoundaries {
		stopAt[s] = true
	}
	var depthPtr *int
	if plan.Depth != nil {
		depthPtr = plan.Depth
	}
	wantClosure := collectClosure(lookup, plan.Wants, depthPtr, nil)
	var haveClosure ClosureResult
	if len(plan.Haves) > 0 {
		haveClosure = collectClosure(lookup, plan.Haves, nil, stopAt)
	} else {
		haveClosure = ClosureResult{Objects: map[string]bool{}, CommitDist: map[string]int{}}
	}
	sendOids := map[string]bool{}
	for oid := range wantClosure.Objects {
		if !haveClosure.Objects[oid] {
			sendOids[oid] = true
		}
	}
	var sortedOids []string
	for oid := range sendOids {
		sortedOids = append(sortedOids, oid)
	}
	sort.Strings(sortedOids)
	var send []*GitObject
	for _, oid := range sortedOids {
		if obj := repo.Objects[oid]; obj != nil {
			send = append(send, obj)
		}
	}
	var shallow []string
	if plan.Depth != nil || len(plan.ShallowBoundaries) > 0 {
		for _, oid := range wantClosure.Boundary {
			if sendOids[oid] || haveClosure.Objects[oid] {
				shallow = append(shallow, oid)
			}
		}
	}
	var unshallow []string
	for _, old := range plan.ShallowBoundaries {
		if wantClosure.Objects[old] {
			found := false
			for _, s := range shallow {
				if s == old {
					found = true
					break
				}
			}
			if !found {
				unshallow = append(unshallow, old)
			}
		}
	}
	var acks []string
	for _, have := range plan.Haves {
		_, peeled := peelObject(lookup, have)
		common := false
		for _, want := range plan.Wants {
			_, wantPeeled := peelObject(lookup, want)
			if peeled == wantPeeled || isAncestor(lookup, peeled, wantPeeled) {
				common = true
				break
			}
		}
		if common {
			acks = append(acks, have)
		}
	}
	return FetchSelection{Send: send, SendOids: sendOids, Shallow: shallow, Unshallow: unshallow, Acks: acks, HasCommon: len(acks) > 0}
}

const sidebandMaxChunk = 65515

const packCacheMax = 128

var packCache = map[string][]byte{}
var packCacheOrder []string

func packFor(sendOids map[string]bool, send []*GitObject) []byte {
	var keys []string
	for oid := range sendOids {
		keys = append(keys, oid)
	}
	sort.Strings(keys)
	key := strings.Join(keys, ",")
	if hit, ok := packCache[key]; ok {
		return hit
	}
	pack := writePack(send)
	if len(packCache) >= packCacheMax && len(packCacheOrder) > 0 {
		oldest := packCacheOrder[0]
		delete(packCache, oldest)
		packCacheOrder = packCacheOrder[1:]
	}
	packCache[key] = pack
	packCacheOrder = append(packCacheOrder, key)
	return pack
}

func sidebandPackets(pack []byte) [][]byte {
	var out [][]byte
	for offset := 0; offset < len(pack); offset += sidebandMaxChunk {
		chunkLen := len(pack) - offset
		if chunkLen > sidebandMaxChunk {
			chunkLen = sidebandMaxChunk
		}
		total := chunkLen + 5
		packet := make([]byte, total)
		copy(packet[0:4], []byte(fmt.Sprintf("%04x", total)))
		packet[4] = 1
		copy(packet[5:], pack[offset:offset+chunkLen])
		out = append(out, packet)
	}
	return out
}

// Whole-response fetch cache (improvement #2): during a clone run the 50
// clones send identical requests to an unchanged repo, so the entire
// framed response is one immutable buffer. Keyed by repo generation, so
// any mutation (refs or objects) invalidates it.
var fetchCache = map[string][]byte{}
var fetchCacheOrder []string

func fetchCacheKey(kind string, repo *Repo, plan FetchPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%d|%d|", kind, repo.ID, repo.Generation)
	b.WriteString(strings.Join(plan.Wants, ","))
	b.WriteString("|")
	b.WriteString(strings.Join(plan.Haves, ","))
	b.WriteString("|")
	b.WriteString(strings.Join(plan.ShallowBoundaries, ","))
	b.WriteString("|")
	if plan.Depth != nil {
		fmt.Fprintf(&b, "%d", *plan.Depth)
	}
	return b.String()
}

func fetchCacheGet(key string) ([]byte, bool) {
	v, ok := fetchCache[key]
	return v, ok
}

func fetchCachePut(key string, body []byte) {
	if len(fetchCache) >= 128 && len(fetchCacheOrder) > 0 {
		oldest := fetchCacheOrder[0]
		delete(fetchCache, oldest)
		fetchCacheOrder = fetchCacheOrder[1:]
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	fetchCache[key] = cp
	fetchCacheOrder = append(fetchCacheOrder, key)
}

func fetchV2(repo *Repo, plan FetchPlan) []byte {
	if hit, ok := fetchCacheGet(fetchCacheKey("v2", repo, plan)); ok {
		return hit
	}
	sel := selectFetch(repo, plan)
	var parts [][]byte
	if len(plan.Haves) > 0 {
		parts = append(parts, pktLine([]byte("acknowledgments\n")))
		if sel.HasCommon {
			for _, ack := range sel.Acks {
				parts = append(parts, pktLine([]byte("ACK "+ack+"\n")))
			}
			parts = append(parts, pktLine([]byte("ready\n")))
		} else {
			parts = append(parts, pktLine([]byte("NAK\n")))
		}
		parts = append(parts, pktDelim())
	}
	if len(sel.Shallow) > 0 || len(sel.Unshallow) > 0 {
		parts = append(parts, pktLine([]byte("shallow-info\n")))
		for _, oid := range sel.Shallow {
			parts = append(parts, pktLine([]byte("shallow "+oid+"\n")))
		}
		for _, oid := range sel.Unshallow {
			parts = append(parts, pktLine([]byte("unshallow "+oid+"\n")))
		}
		parts = append(parts, pktDelim())
	}
	if len(sel.Send) > 0 {
		parts = append(parts, pktLine([]byte("packfile\n")))
		pack := packFor(sel.SendOids, sel.Send)
		for _, p := range sidebandPackets(pack) {
			parts = append(parts, p)
		}
	}
	parts = append(parts, pktFlush())
	body := bytes.Join(parts, nil)
	fetchCachePut(fetchCacheKey("v2", repo, plan), body)
	return body
}

func fetchV0(repo *Repo, plan FetchPlan) []byte {
	if hit, ok := fetchCacheGet(fetchCacheKey("v0", repo, plan)); ok {
		return hit
	}
	sel := selectFetch(repo, plan)
	var parts [][]byte
	if sel.HasCommon {
		for _, ack := range sel.Acks {
			parts = append(parts, pktLine([]byte("ACK "+ack+"\n")))
		}
	} else {
		parts = append(parts, pktLine([]byte("NAK\n")))
	}
	for _, oid := range sel.Shallow {
		parts = append(parts, pktLine([]byte("shallow "+oid+"\n")))
	}
	for _, oid := range sel.Unshallow {
		parts = append(parts, pktLine([]byte("unshallow "+oid+"\n")))
	}
	if len(sel.Send) > 0 {
		pack := packFor(sel.SendOids, sel.Send)
		for _, p := range sidebandPackets(pack) {
			parts = append(parts, p)
		}
	}
	parts = append(parts, pktFlush())
	body := bytes.Join(parts, nil)
	fetchCachePut(fetchCacheKey("v0", repo, plan), body)
	return body
}

// ---------------------------------------------------------------------------
// upload-pack / receive-pack
// ---------------------------------------------------------------------------

type v2Request struct {
	Command string
	Args    map[string][]string
}

func parseV2Request(body []byte) (v2Request, error) {
	packets, _, err := decodePktLines(body)
	if err != nil {
		return v2Request{}, err
	}
	out := v2Request{Args: map[string][]string{}}
	afterDelim := false
	for _, packet := range packets {
		if packet.Kind == "flush" {
			break
		}
		if packet.Kind == "delim" {
			afterDelim = true
			continue
		}
		line := string(packet.Payload)
		if !afterDelim {
			if strings.HasPrefix(line, "command=") {
				out.Command = strings.TrimRight(strings.TrimPrefix(line, "command="), "\n")
			}
			continue
		}
		trimmed := strings.TrimSuffix(line, "\n")
		space := strings.Index(trimmed, " ")
		key, value := trimmed, ""
		if space >= 0 {
			key, value = trimmed[:space], trimmed[space+1:]
		}
		out.Args[key] = append(out.Args[key], value)
	}
	return out, nil
}

func handleUploadPack(store *Store, repo *Repo, req *GitRequest) GitResponse {
	protocol := req.Headers["git-protocol"]
	if protocol == "version=2" {
		parsed, err := parseV2Request(req.Body)
		if err != nil {
			return GitResponse{Status: 400, ContentType: "text/plain; charset=utf-8", Body: []byte("bad pkt-line\n")}
		}
		if parsed.Command == "ls-refs" {
			args := parsed.Args
			body := lsRefsV2(repo, hasKey(args, "peel"), hasKey(args, "symrefs"), hasKey(args, "unborn"), args["ref-prefix"])
			return GitResponse{Status: 200, ContentType: uploadResult, Body: body}
		}
		if parsed.Command == "fetch" {
			args := parsed.Args
			var depth *int
			if raw, ok := args["deepen"]; ok && len(raw) > 0 {
				var d int
				fmt.Sscanf(raw[0], "%d", &d)
				depth = &d
			}
			plan := FetchPlan{Wants: args["want"], Haves: args["have"], ShallowBoundaries: args["shallow"], Depth: depth}
			return GitResponse{Status: 200, ContentType: uploadResult, Body: fetchV2(repo, plan)}
		}
		return GitResponse{Status: 400, ContentType: "text/plain; charset=utf-8", Body: []byte("unknown command\n")}
	}
	// Protocol v0: decode past the first flush to reach the haves section.
	var packets []DecodedPacket
	offset := 0
	for offset < len(req.Body) {
		decoded, consumed, err := decodePktLines(req.Body[offset:])
		if err != nil {
			return GitResponse{Status: 400, ContentType: "text/plain; charset=utf-8", Body: []byte("bad pkt-line\n")}
		}
		packets = append(packets, decoded...)
		if consumed <= 0 {
			break
		}
		offset += consumed
	}
	var wants, haves, shallow []string
	var depth *int
	for _, packet := range packets {
		if packet.Kind == "flush" {
			continue
		}
		if packet.Kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(packet.Payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, strings.Split(line[5:], " ")[0])
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			var d int
			fmt.Sscanf(strings.TrimSpace(line[7:]), "%d", &d)
			depth = &d
		}
	}
	return GitResponse{Status: 200, ContentType: uploadResult, Body: fetchV0(repo, FetchPlan{Wants: wants, Haves: haves, ShallowBoundaries: shallow, Depth: depth})}
}

func hasKey(m map[string][]string, k string) bool {
	_, ok := m[k]
	return ok
}

type PushCommand struct {
	OldOID string
	NewOID string
	Ref    string
}

func handleReceivePack(store *Store, repo *Repo, req *GitRequest) GitResponse {
	var commands []PushCommand
	packStart := 0
	decoded, consumed, err := decodePktLines(req.Body)
	if err != nil {
		return GitResponse{Status: 400, ContentType: "text/plain; charset=utf-8", Body: []byte("bad pkt-line\n")}
	}
	packStart = consumed
	for _, packet := range decoded {
		if packet.Kind != "data" {
			continue
		}
		line := string(packet.Payload)
		if nul := strings.Index(line, "\x00"); nul >= 0 {
			line = line[:nul]
		}
		clean := strings.TrimSuffix(line, "\n")
		parts := strings.Split(clean, " ")
		if len(parts) >= 3 {
			commands = append(commands, PushCommand{OldOID: parts[0], NewOID: parts[1], Ref: parts[2]})
		}
	}
	if len(commands) == 0 {
		return GitResponse{Status: 400, ContentType: "text/plain; charset=utf-8", Body: []byte("no commands\n")}
	}
	hasUpdates := false
	for _, c := range commands {
		if c.NewOID != ZeroOID {
			hasUpdates = true
		}
	}
	unpackOk := true
	unpackDetail := "ok"
	if hasUpdates && packStart < len(req.Body) {
		objects, err := readPack(req.Body[packStart:], func(oid string) *GitObject { return repo.Objects[oid] })
		if err != nil {
			unpackOk = false
			unpackDetail = err.Error()
		} else {
			for oid, obj := range objects {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = obj
					store.bumpGen(repo)
				}
			}
		}
	} else if hasUpdates && packStart >= len(req.Body) {
		unpackOk = false
		unpackDetail = "unpacker error: no packfile"
	}
	var reportLines []string
	if !unpackOk {
		reportLines = append(reportLines, "unpack "+unpackDetail+"\n")
		for _, cmd := range commands {
			reportLines = append(reportLines, "ng "+cmd.Ref+" unpack failed\n")
		}
	} else {
		reportLines = append(reportLines, "unpack ok\n")
		for _, cmd := range commands {
			result := applyPushCommand(store, repo, cmd)
			if result.OK {
				reportLines = append(reportLines, "ok "+cmd.Ref+"\n")
			} else {
				reportLines = append(reportLines, "ng "+cmd.Ref+" "+result.Reason+"\n")
			}
		}
	}
	reportBody := append(encodePktLines(reportLines), pktFlush()...)
	var parts [][]byte
	for offset := 0; offset < len(reportBody); offset += sidebandMaxChunk {
		chunkLen := len(reportBody) - offset
		if chunkLen > sidebandMaxChunk {
			chunkLen = sidebandMaxChunk
		}
		total := chunkLen + 5
		packet := make([]byte, total)
		copy(packet[0:4], []byte(fmt.Sprintf("%04x", total)))
		packet[4] = 1
		copy(packet[5:], reportBody[offset:offset+chunkLen])
		parts = append(parts, packet)
	}
	parts = append(parts, pktFlush())
	return GitResponse{Status: 200, ContentType: receiveResult, Body: bytes.Join(parts, nil)}
}

type pushResult struct {
	OK     bool
	Reason string
}

func applyPushCommand(store *Store, repo *Repo, cmd PushCommand) pushResult {
	if !isObjectId(cmd.OldOID) || !isObjectId(cmd.NewOID) {
		return pushResult{OK: false, Reason: "invalid object id"}
	}
	current, exists := repo.Refs[cmd.Ref]
	if !exists {
		if cmd.OldOID != ZeroOID {
			return pushResult{OK: false, Reason: "non-fast-forward"}
		}
		if cmd.NewOID == ZeroOID {
			return pushResult{OK: false, Reason: "cannot delete a ref that does not exist"}
		}
		if _, ok := repo.Objects[cmd.NewOID]; !ok {
			return pushResult{OK: false, Reason: "object not found"}
		}
		store.SetRef(repo, cmd.Ref, cmd.NewOID)
		return pushResult{OK: true}
	}
	if cmd.OldOID != current {
		return pushResult{OK: false, Reason: "non-fast-forward"}
	}
	if cmd.NewOID == ZeroOID {
		store.DeleteRef(repo, cmd.Ref)
		return pushResult{OK: true}
	}
	if cmd.NewOID == current {
		return pushResult{OK: true}
	}
	if _, ok := repo.Objects[cmd.NewOID]; !ok {
		return pushResult{OK: false, Reason: "object not found"}
	}
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	if !isAncestor(lookup, current, cmd.NewOID) {
		return pushResult{OK: false, Reason: "non-fast-forward"}
	}
	store.SetRef(repo, cmd.Ref, cmd.NewOID)
	return pushResult{OK: true}
}

func handleGit(store *Store, req *GitRequest) *GitResponse {
	segments := splitNonEmpty(req.Path, "/")
	if len(segments) < 2 {
		return nil
	}
	repoSegment := segments[1]
	if !strings.HasSuffix(repoSegment, ".git") {
		return nil
	}
	owner := segments[0]
	repoName := repoSegment[:len(repoSegment)-4]
	rest := segments[2:]
	repo := store.ResolveRepo(owner, repoName)
	if repo == nil {
		body := []byte(jsonString(map[string]any{"message": "Not Found"}))
		return &GitResponse{Status: 404, ContentType: "application/json; charset=utf-8", Body: body}
	}
	if req.Method == "GET" && len(rest) == 2 && rest[0] == "info" && rest[1] == "refs" {
		service := req.QueryGet("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			body := []byte(jsonString(map[string]any{"message": "unknown service"}))
			return &GitResponse{Status: 400, ContentType: "application/json; charset=utf-8", Body: body}
		}
		r := infoRefs(repo, service, req.Headers["git-protocol"])
		return &r
	}
	if req.Method == "POST" && len(rest) == 1 && rest[0] == "git-upload-pack" {
		r := handleUploadPack(store, repo, req)
		return &r
	}
	if req.Method == "POST" && len(rest) == 1 && rest[0] == "git-receive-pack" {
		r := handleReceivePack(store, repo, req)
		return &r
	}
	body := []byte(jsonString(map[string]any{"message": "Not Found"}))
	return &GitResponse{Status: 404, ContentType: "application/json; charset=utf-8", Body: body}
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
