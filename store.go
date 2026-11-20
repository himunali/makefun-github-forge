// The forge's whole state: one in-memory store, no clock, no randomness.
// Port of entry/store.ts. Same logical timestamps (MAKEFUN_TRIAL_NOW +
// a per-mutation counter), same sequential ids, same JSON views.
package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const DefaultBranch = "main"

func parseTrialNow(raw string) int64 {
	if raw == "" {
		return 1735689600
	}
	trimmed := strings.TrimSpace(raw)
	if matched, _ := regexp.MatchString(`^-?\d+$`, trimmed); matched {
		n, err := strconv.ParseInt(trimmed, 10, 64)
		if err == nil {
			if n > 100000000000 {
				return n / 1000
			}
			return n
		}
	}
	if ms, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return ms.Unix()
	}
	if t, err := time.Parse("2006-01-02T15:04:05Z", trimmed); err == nil {
		return t.Unix()
	}
	return 1735689600
}

// Format epoch seconds as ISO-8601 UTC (Hinnant civil-date algorithm).
func isoFromSec(sec int64) string {
	days := sec / 86400
	rem := sec - days*86400
	if rem < 0 {
		// sec is never negative in practice (trial base 2025); keep the
		// TS behaviour (floor division) for safety.
		days--
		rem += 86400
	}
	hh := rem / 3600
	mm := (rem - hh*3600) / 60
	ss := rem - hh*3600 - mm*60
	z := days + 719468
	era := z / 146097
	if z < 0 && z%146097 != 0 {
		era--
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	year := y
	if m <= 2 {
		year = y + 1
	}
	pad := func(n int64, w int) string {
		s := strconv.FormatInt(n, 10)
		for len(s) < w {
			s = "0" + s
		}
		return s
	}
	return fmt.Sprintf("%s-%s-%sT%s:%s:%sZ", pad(year, 4), pad(m, 2), pad(d, 2), pad(hh, 2), pad(mm, 2), pad(ss, 2))
}

type Label struct {
	ID          int
	Name        string
	Color       string
	Description string
}

type Comment struct {
	ID         int
	Body       string
	CreatedSeq int
	CreatedAt  string
	UpdatedAt  string
}

type Issue struct {
	Number     int
	ID         int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []*Label
	Comments   []*Comment
	CreatedSeq int
	CreatedAt  string
	UpdatedAt  string
	ClosedAt   *string
}

type PullRequest struct {
	Number         int
	ID             int
	Title          string
	Body           string
	State          string
	Head           string
	Base           string
	HeadSha        string
	BaseSha        string
	Merged         bool
	MergedAt       *string
	MergeCommitSha *string
	CreatedSeq     int
	CreatedAt      string
	UpdatedAt      string
	ClosedAt       *string
}

type Repo struct {
	ID            int
	Name          string
	Owner         *string
	Description   string
	IsPrivate     bool
	DefaultBranch string
	Objects       map[string]*GitObject
	Refs          map[string]string
	Labels        map[string]*Label
	Issues        map[int]*Issue
	Pulls         map[int]*PullRequest
	NextNumber    int
	CreatedSeq    int
	CreatedAt     string
	UpdatedAt     string
	PushedAt      *string
	Generation    int
}

type Store struct {
	Token        string
	BaseSec      int64
	seq          int
	// NextRequestId is called on EVERY request, including read-only ones
	// that now run concurrently under the read lock — so it must be
	// atomic, not a plain increment behind the old global mutex.
	reqSeq atomic.Uint64
	repoIdSeq    int
	objectIdSeq  int
	labelIdSeq   int
	commentIdSeq int
	ReposByName  map[string]*Repo
	ReposById    map[int]*Repo
	ViewerLogin  *string
}

func NewStore() *Store {
	return &Store{
		Token:       os.Getenv("MAKEFUN_SHOP_TOKEN"),
		BaseSec:     parseTrialNow(os.Getenv("MAKEFUN_TRIAL_NOW")),
		ReposByName: map[string]*Repo{},
		ReposById:   map[int]*Repo{},
	}
}

func (s *Store) TickSec() int64 {
	s.seq++
	return s.BaseSec + int64(s.seq)
}

func (s *Store) NextRequestId() string {
	return fmt.Sprintf("req-%d", s.reqSeq.Add(1))
}

func (s *Store) ViewerIdentity() string {
	login := "owner"
	if s.ViewerLogin != nil {
		login = *s.ViewerLogin
	}
	return fmt.Sprintf("%s <%s@example.invalid>", login, login)
}

func (s *Store) OwnerLogin(repo *Repo) string {
	if repo.Owner != nil {
		return *repo.Owner
	}
	if s.ViewerLogin != nil {
		return *s.ViewerLogin
	}
	return "owner"
}

func (s *Store) bumpGen(repo *Repo) { repo.Generation++ }

func (s *Store) CreateRepo(name, description string, isPrivate bool) *Repo {
	if _, ok := s.ReposByName[name]; ok {
		return nil
	}
	sec := s.TickSec()
	s.repoIdSeq++
	repo := &Repo{
		ID: s.repoIdSeq, Name: name, Owner: s.ViewerLogin, Description: description,
		IsPrivate: isPrivate, DefaultBranch: DefaultBranch,
		Objects: map[string]*GitObject{}, Refs: map[string]string{},
		Labels: map[string]*Label{}, Issues: map[int]*Issue{}, Pulls: map[int]*PullRequest{},
		NextNumber: 1, CreatedSeq: s.seq, CreatedAt: isoFromSec(sec), UpdatedAt: isoFromSec(sec),
	}
	s.ReposByName[name] = repo
	s.ReposById[repo.ID] = repo
	return repo
}

func (s *Store) DeleteRepo(repo *Repo) {
	delete(s.ReposByName, repo.Name)
	delete(s.ReposById, repo.ID)
}

func (s *Store) BindOwner(repo *Repo, owner string) bool {
	if repo.Owner == nil {
		repo.Owner = &owner
		if s.ViewerLogin == nil {
			s.ViewerLogin = &owner
		}
		return true
	}
	return *repo.Owner == owner
}

func (s *Store) ResolveRepo(owner, name string) *Repo {
	repo, ok := s.ReposByName[name]
	if !ok {
		return nil
	}
	if !s.BindOwner(repo, owner) {
		return nil
	}
	return repo
}

func (s *Store) RepoByName(name string) *Repo { return s.ReposByName[name] }

func (s *Store) ListRepos() []*Repo {
	out := make([]*Repo, 0, len(s.ReposByName))
	for _, r := range s.ReposByName {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedSeq < out[j].CreatedSeq })
	return out
}

func (s *Store) RefTip(repo *Repo, ref string) (string, bool) {
	v, ok := repo.Refs[ref]
	return v, ok
}

func (s *Store) SetRef(repo *Repo, ref, oid string) {
	repo.Refs[ref] = oid
	s.bumpGen(repo)
}

func (s *Store) DeleteRef(repo *Repo, ref string) {
	delete(repo.Refs, ref)
	s.bumpGen(repo)
}

func (s *Store) Lookup(repo *Repo) ObjectLookup {
	return func(oid string) *GitObject { return repo.Objects[oid] }
}

func (s *Store) PutObject(repo *Repo, typ GitObjectType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = &GitObject{Type: typ, Data: data}
		s.bumpGen(repo)
	}
	return oid
}

func (s *Store) RefCommit(repo *Repo, ref string) string {
	tip, ok := repo.Refs[ref]
	if !ok {
		return ""
	}
	_, oid := peelObject(s.Lookup(repo), tip)
	return oid
}

func (s *Store) BranchNames(repo *Repo) []string {
	var names []string
	for ref := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, ref[len("refs/heads/"):])
		}
	}
	sort.Strings(names)
	return names
}

// ------------------------------------------------------------------ trees/files

type FileEntry struct {
	OID  string
	Mode string
}

func (s *Store) BuildTreeFromFiles(repo *Repo, files map[string]FileEntry) string {
	type Node struct {
		files map[string]FileEntry
		dirs  map[string]*Node
	}
	makeNode := func() *Node { return &Node{files: map[string]FileEntry{}, dirs: map[string]*Node{}} }
	root := makeNode()
	for path, entry := range files {
		segments := strings.Split(path, "/")
		node := root
		for i := 0; i < len(segments)-1; i++ {
			seg := segments[i]
			if _, ok := node.dirs[seg]; !ok {
				node.dirs[seg] = makeNode()
			}
			node = node.dirs[seg]
		}
		node.files[segments[len(segments)-1]] = entry
	}
	var build func(node *Node) string
	build = func(node *Node) string {
		var entries []TreeEntry
		for name, e := range node.files {
			entries = append(entries, TreeEntry{Mode: e.Mode, Name: name, OID: e.OID})
		}
		for name, child := range node.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return s.PutObject(repo, ObjTree, buildTree(entries))
	}
	return build(root)
}

func (s *Store) FilesAtCommit(repo *Repo, commitOid string) map[string]FileEntry {
	obj := repo.Objects[commitOid]
	if obj == nil || obj.Type != ObjCommit {
		return map[string]FileEntry{}
	}
	tree := parseCommit(obj.Data).Tree
	flat := flattenTree(s.Lookup(repo), tree, "")
	out := map[string]FileEntry{}
	for path, e := range flat {
		out[path] = FileEntry{OID: e.OID, Mode: e.Mode}
	}
	return out
}

func (s *Store) FilesAtRef(repo *Repo, ref string) map[string]FileEntry {
	commit := s.RefCommit(repo, ref)
	if commit == "" {
		return map[string]FileEntry{}
	}
	return s.FilesAtCommit(repo, commit)
}

type CommitFileResult struct {
	OK        bool
	Status    int
	Message   string
	CommitSha string
	BlobSha   string
	Created   bool
}

func (s *Store) CommitFile(repo *Repo, path string, content []byte, message, branch, sha string, hasSha bool) CommitFileResult {
	branchRef := "refs/heads/" + branch
	parent := s.RefCommit(repo, branchRef)
	if parent == "" && branch != repo.DefaultBranch {
		parent = s.RefCommit(repo, "refs/heads/"+repo.DefaultBranch)
	}
	var files map[string]FileEntry
	if parent != "" {
		files = s.FilesAtCommit(repo, parent)
	} else {
		files = map[string]FileEntry{}
	}
	existing, exists := files[path]
	if exists && hasSha && !strings.EqualFold(sha, existing.OID) {
		return CommitFileResult{OK: false, Status: 409, Message: "sha does not match the current file (stale sha)"}
	}
	if !exists && hasSha {
		return CommitFileResult{OK: false, Status: 409, Message: "sha was given but the file does not exist"}
	}
	blobSha := s.PutObject(repo, ObjBlob, content)
	files[path] = FileEntry{OID: blobSha, Mode: "100644"}
	treeOid := s.BuildTreeFromFiles(repo, files)
	sec := s.TickSec()
	identity := s.ViewerIdentity()
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	commitSha := s.PutObject(repo, ObjCommit, buildCommit(treeOid, parents, identity, identity, sec, message))
	repo.Refs[branchRef] = commitSha
	s.bumpGen(repo)
	repo.UpdatedAt = isoFromSec(sec)
	t := isoFromSec(sec)
	repo.PushedAt = &t
	return CommitFileResult{OK: true, CommitSha: commitSha, BlobSha: blobSha, Created: !exists}
}

type DeleteFileResult struct {
	OK        bool
	Status    int
	Message   string
	CommitSha string
}

func (s *Store) DeleteFile(repo *Repo, path, message, branch, sha string, hasSha bool) DeleteFileResult {
	branchRef := "refs/heads/" + branch
	parent := s.RefCommit(repo, branchRef)
	if parent == "" {
		return DeleteFileResult{OK: false, Status: 404, Message: "Not Found"}
	}
	files := s.FilesAtCommit(repo, parent)
	existing, exists := files[path]
	if !exists {
		return DeleteFileResult{OK: false, Status: 404, Message: "Not Found"}
	}
	if hasSha && !strings.EqualFold(sha, existing.OID) {
		return DeleteFileResult{OK: false, Status: 409, Message: "sha does not match the current file (stale sha)"}
	}
	delete(files, path)
	treeOid := s.BuildTreeFromFiles(repo, files)
	sec := s.TickSec()
	identity := s.ViewerIdentity()
	commitSha := s.PutObject(repo, ObjCommit, buildCommit(treeOid, []string{parent}, identity, identity, sec, message))
	repo.Refs[branchRef] = commitSha
	s.bumpGen(repo)
	repo.UpdatedAt = isoFromSec(sec)
	t := isoFromSec(sec)
	repo.PushedAt = &t
	return DeleteFileResult{OK: true, CommitSha: commitSha}
}

// ------------------------------------------------------------------ labels/issues

func (s *Store) CreateLabel(repo *Repo, name, color, description string) *Label {
	if existing, ok := repo.Labels[name]; ok {
		return existing
	}
	if color == "" {
		color = "ededed"
	}
	s.labelIdSeq++
	label := &Label{ID: s.labelIdSeq, Name: name, Color: color, Description: description}
	repo.Labels[name] = label
	return label
}

func (s *Store) CreateIssue(repo *Repo, title, body string, labelNames []string) (*Issue, string) {
	var labels []*Label
	for _, name := range labelNames {
		label, ok := repo.Labels[name]
		if !ok {
			return nil, fmt.Sprintf("Label does not exist: %s", name)
		}
		labels = append(labels, label)
	}
	number := repo.NextNumber
	repo.NextNumber++
	sec := s.TickSec()
	s.objectIdSeq++
	issue := &Issue{
		Number: number, ID: s.objectIdSeq, Title: title, Body: body, State: "open",
		Labels: labels, Comments: []*Comment{},
		CreatedSeq: s.seq, CreatedAt: isoFromSec(sec), UpdatedAt: isoFromSec(sec),
	}
	repo.Issues[number] = issue
	return issue, ""
}

func (s *Store) AddComment(repo *Repo, issue *Issue, body string) *Comment {
	sec := s.TickSec()
	s.commentIdSeq++
	comment := &Comment{ID: s.commentIdSeq, Body: body, CreatedSeq: s.seq, CreatedAt: isoFromSec(sec), UpdatedAt: isoFromSec(sec)}
	issue.Comments = append(issue.Comments, comment)
	issue.UpdatedAt = isoFromSec(sec)
	return comment
}

func (s *Store) CloseIssue(repo *Repo, issue *Issue) {
	if issue.State == "closed" {
		return
	}
	sec := s.TickSec()
	issue.State = "closed"
	t := isoFromSec(sec)
	issue.ClosedAt = &t
	issue.UpdatedAt = isoFromSec(sec)
}

func (s *Store) ReopenIssue(repo *Repo, issue *Issue) {
	if issue.State == "open" {
		return
	}
	sec := s.TickSec()
	issue.State = "open"
	issue.ClosedAt = nil
	issue.UpdatedAt = isoFromSec(sec)
}

func (s *Store) ListIssues(repo *Repo, state string) []*Issue {
	var out []*Issue
	for _, issue := range repo.Issues {
		if state == "all" || issue.State == state {
			out = append(out, issue)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// -------------------------------------------------------------------- pulls

func (s *Store) CreatePull(repo *Repo, title, body, head, base string) (*PullRequest, string) {
	headRef := "refs/heads/" + head
	baseRef := "refs/heads/" + base
	headTip, ok1 := repo.Refs[headRef]
	baseTip, ok2 := repo.Refs[baseRef]
	if !ok1 {
		return nil, fmt.Sprintf("head branch %s does not exist", head)
	}
	if !ok2 {
		return nil, fmt.Sprintf("base branch %s does not exist", base)
	}
	for _, pr := range repo.Pulls {
		if pr.State == "open" && pr.Head == head && pr.Base == base {
			return nil, "A pull request already exists for this head and base"
		}
	}
	number := repo.NextNumber
	repo.NextNumber++
	sec := s.TickSec()
	s.objectIdSeq++
	_, headSha := peelObject(s.Lookup(repo), headTip)
	_, baseSha := peelObject(s.Lookup(repo), baseTip)
	pr := &PullRequest{
		Number: number, ID: s.objectIdSeq, Title: title, Body: body, State: "open",
		Head: head, Base: base, HeadSha: headSha, BaseSha: baseSha,
		CreatedSeq: s.seq, CreatedAt: isoFromSec(sec), UpdatedAt: isoFromSec(sec),
	}
	repo.Pulls[number] = pr
	return pr, ""
}

type PRFile struct {
	Path      string
	Status    string
	Additions int
	Deletions int
	Changes   int
}

func (s *Store) PRFiles(repo *Repo, pr *PullRequest) []PRFile {
	headCommit := s.RefCommit(repo, "refs/heads/"+pr.Head)
	baseCommit := s.RefCommit(repo, "refs/heads/"+pr.Base)
	if headCommit == "" || baseCommit == "" {
		return nil
	}
	ancestor := mergeBase(s.Lookup(repo), headCommit, baseCommit)
	if ancestor == "" {
		ancestor = baseCommit
	}
	baseFiles := s.FilesAtCommit(repo, ancestor)
	headFiles := s.FilesAtCommit(repo, headCommit)
	pathsSet := map[string]bool{}
	for p := range baseFiles {
		pathsSet[p] = true
	}
	for p := range headFiles {
		pathsSet[p] = true
	}
	var paths []string
	for p := range pathsSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []PRFile
	for _, path := range paths {
		before, hasBefore := baseFiles[path]
		after, hasAfter := headFiles[path]
		if hasBefore && hasAfter && before.OID == after.OID {
			continue
		}
		status := "modified"
		if !hasBefore {
			status = "added"
		} else if !hasAfter {
			status = "removed"
		}
		beforeText := ""
		if hasBefore {
			if o := repo.Objects[before.OID]; o != nil {
				beforeText = string(o.Data)
			}
		}
		afterText := ""
		if hasAfter {
			if o := repo.Objects[after.OID]; o != nil {
				afterText = string(o.Data)
			}
		}
		add, del := lineDiff(beforeText, afterText)
		out = append(out, PRFile{Path: path, Status: status, Additions: add, Deletions: del, Changes: add + del})
	}
	return out
}

func (s *Store) PRMergeable(repo *Repo, pr *PullRequest) string {
	if s.MergeConflict(repo, pr) == nil {
		return "MERGEABLE"
	}
	return "CONFLICTING"
}

func (s *Store) MergeConflict(repo *Repo, pr *PullRequest) []string {
	headCommit := s.RefCommit(repo, "refs/heads/"+pr.Head)
	baseCommit := s.RefCommit(repo, "refs/heads/"+pr.Base)
	if headCommit == "" || baseCommit == "" {
		return []string{"(missing branch)"}
	}
	ancestor := mergeBase(s.Lookup(repo), headCommit, baseCommit)
	if ancestor == "" {
		ancestor = baseCommit
	}
	baseFiles := s.FilesAtCommit(repo, ancestor)
	headFiles := s.FilesAtCommit(repo, headCommit)
	tipFiles := s.FilesAtCommit(repo, baseCommit)
	pathsSet := map[string]bool{}
	for p := range baseFiles {
		pathsSet[p] = true
	}
	for p := range headFiles {
		pathsSet[p] = true
	}
	for p := range tipFiles {
		pathsSet[p] = true
	}
	var conflicts []string
	for path := range pathsSet {
		anc, hasAnc := baseFiles[path]
		head, hasHead := headFiles[path]
		tip, hasTip := tipFiles[path]
		ancOID := ""
		if hasAnc {
			ancOID = anc.OID
		}
		headOID := ""
		if hasHead {
			headOID = head.OID
		}
		tipOID := ""
		if hasTip {
			tipOID = tip.OID
		}
		headChanged := headOID != ancOID
		tipChanged := tipOID != ancOID
		if headChanged && tipChanged && headOID != tipOID {
			conflicts = append(conflicts, path)
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	return conflicts
}

type MergePullResult struct {
	OK      bool
	Status  int
	Message string
	Sha     string
}

func (s *Store) MergePull(repo *Repo, pr *PullRequest) MergePullResult {
	if pr.State == "closed" {
		if pr.Merged {
			return MergePullResult{OK: false, Status: 405, Message: "Pull request is already merged"}
		}
		return MergePullResult{OK: false, Status: 405, Message: "Pull request is closed"}
	}
	if conflict := s.MergeConflict(repo, pr); conflict != nil {
		return MergePullResult{OK: false, Status: 405, Message: "Merge conflict in " + strings.Join(conflict, ", ")}
	}
	headCommit := s.RefCommit(repo, "refs/heads/"+pr.Head)
	baseCommit := s.RefCommit(repo, "refs/heads/"+pr.Base)
	if headCommit == "" || baseCommit == "" {
		return MergePullResult{OK: false, Status: 404, Message: "Not Found"}
	}
	ancestor := mergeBase(s.Lookup(repo), headCommit, baseCommit)
	if ancestor == "" {
		ancestor = baseCommit
	}
	baseFiles := s.FilesAtCommit(repo, ancestor)
	headFiles := s.FilesAtCommit(repo, headCommit)
	merged := s.FilesAtCommit(repo, baseCommit)
	for path, e := range headFiles {
		merged[path] = e
	}
	for path := range baseFiles {
		if _, ok := headFiles[path]; !ok {
			delete(merged, path)
		}
	}
	treeOid := s.BuildTreeFromFiles(repo, merged)
	sec := s.TickSec()
	identity := s.ViewerIdentity()
	message := fmt.Sprintf("%s (#%d)", pr.Title, pr.Number)
	commitSha := s.PutObject(repo, ObjCommit, buildCommit(treeOid, []string{baseCommit}, identity, identity, sec, message))
	repo.Refs["refs/heads/"+pr.Base] = commitSha
	s.bumpGen(repo)
	pr.State = "closed"
	pr.Merged = true
	t := isoFromSec(sec)
	pr.MergedAt = &t
	pr.MergeCommitSha = &commitSha
	pr.ClosedAt = &t
	pr.UpdatedAt = isoFromSec(sec)
	pr.BaseSha = commitSha
	repo.UpdatedAt = isoFromSec(sec)
	repo.PushedAt = &t
	return MergePullResult{OK: true, Sha: commitSha}
}

// -------------------------------------------------------------- global ids

func (s *Store) GlobalId(kind string, parts ...any) string {
	strs := make([]string, len(parts))
	for i, p := range parts {
		strs[i] = fmt.Sprintf("%v", p)
	}
	return globalIdEncode(kind + ":" + strings.Join(strs, ":"))
}

func (s *Store) DecodeGlobalId(id string) (kind string, parts []string, ok bool) {
	text, err := globalIdDecode(id)
	if err != nil {
		return "", nil, false
	}
	segs := strings.Split(text, ":")
	if len(segs) < 2 || segs[0] == "" {
		return "", nil, false
	}
	return segs[0], segs[1:], true
}

func (s *Store) RepoGlobalId(repo *Repo) string { return s.GlobalId("Repository", repo.ID) }
func (s *Store) IssueGlobalId(repo *Repo, issue *Issue) string {
	return s.GlobalId("Issue", repo.ID, issue.Number)
}
func (s *Store) PullGlobalId(repo *Repo, pr *PullRequest) string {
	return s.GlobalId("PullRequest", repo.ID, pr.Number)
}
func (s *Store) CommentGlobalId(repo *Repo, issue *Issue, comment *Comment) string {
	return s.GlobalId("IssueComment", repo.ID, issue.Number, comment.ID)
}

func (s *Store) CountPullCommits(repo *Repo, pr *PullRequest) int {
	headCommit := s.RefCommit(repo, "refs/heads/"+pr.Head)
	baseCommit := s.RefCommit(repo, "refs/heads/"+pr.Base)
	if headCommit == "" || baseCommit == "" {
		return 0
	}
	ancestor := mergeBase(s.Lookup(repo), headCommit, baseCommit)
	headSet := map[string]bool{}
	for _, oid := range listCommits(s.Lookup(repo), headCommit) {
		headSet[oid] = true
	}
	if ancestor == "" {
		return len(headSet)
	}
	baseSet := map[string]bool{}
	for _, oid := range listCommits(s.Lookup(repo), baseCommit) {
		baseSet[oid] = true
	}
	count := 0
	for oid := range headSet {
		if !baseSet[oid] {
			count++
		}
	}
	return count
}

func lineDiff(before, after string) (additions, deletions int) {
	countLines := func(text string) map[string]int {
		m := map[string]int{}
		if text == "" {
			return m
		}
		for _, line := range strings.Split(text, "\n") {
			m[line]++
		}
		return m
	}
	a := countLines(before)
	b := countLines(after)
	for line, n := range b {
		if had := a[line]; n > had {
			additions += n - had
		}
	}
	for line, n := range a {
		if has := b[line]; n > has {
			deletions += n - has
		}
	}
	return additions, deletions
}

// --------------------------------------------------------------- JSON views

func (s *Store) RepoJson(repo *Repo) map[string]any {
	owner := s.OwnerLogin(repo)
	openCount := 0
	for _, i := range repo.Issues {
		if i.State == "open" {
			openCount++
		}
	}
	return map[string]any{
		"id": repo.ID, "node_id": s.RepoGlobalId(repo), "name": repo.Name,
		"full_name": owner + "/" + repo.Name,
		"owner":     map[string]any{"login": owner, "id": 1000, "type": "User"},
		"private":   repo.IsPrivate, "description": repo.Description,
		"default_branch": repo.DefaultBranch,
		"html_url":       "https://example.invalid/" + owner + "/" + repo.Name,
		"url":            "/api/v3/repos/" + owner + "/" + repo.Name,
		"clone_url":      "https://example.invalid/" + owner + "/" + repo.Name + ".git",
		"git_url":        "git://example.invalid/" + owner + "/" + repo.Name + ".git",
		"ssh_url":        "git@example.invalid:" + owner + "/" + repo.Name + ".git",
		"created_at":     repo.CreatedAt, "updated_at": repo.UpdatedAt, "pushed_at": repo.PushedAt,
		"has_issues": true, "has_projects": true, "has_wiki": true, "fork": false,
		"archived": false, "disabled": false, "license": nil, "topics": []any{},
		"visibility": map[bool]string{true: "private", false: "public"}[repo.IsPrivate],
		"homepage":   nil, "language": nil, "forks_count": 0, "stargazers_count": 0,
		"watchers_count": 0, "open_issues_count": openCount,
		"permissions": map[string]any{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true},
	}
}

func (s *Store) LabelJson(label *Label) map[string]any {
	return map[string]any{
		"id": label.ID, "node_id": s.GlobalId("Label", label.ID), "name": label.Name,
		"color": label.Color, "description": label.Description, "default": false,
		"url": fmt.Sprintf("/api/v3/labels/%d", label.ID),
	}
}

func (s *Store) IssueJson(repo *Repo, issue *Issue) map[string]any {
	owner := s.OwnerLogin(repo)
	var labels []any
	for _, l := range issue.Labels {
		labels = append(labels, s.LabelJson(l))
	}
	if labels == nil {
		labels = []any{}
	}
	return map[string]any{
		"id": issue.ID, "node_id": s.IssueGlobalId(repo, issue), "number": issue.Number,
		"title": issue.Title, "body": issue.Body, "state": issue.State,
		"labels":   labels,
		"user":     map[string]any{"login": owner, "id": 1000, "type": "User"},
		"assignee": nil, "assignees": []any{}, "locked": false,
		"comments":   len(issue.Comments),
		"created_at": issue.CreatedAt, "updated_at": issue.UpdatedAt, "closed_at": issue.ClosedAt,
		"html_url":           fmt.Sprintf("https://example.invalid/%s/%s/issues/%d", owner, repo.Name, issue.Number),
		"url":                fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", owner, repo.Name, issue.Number),
		"repository_url":     fmt.Sprintf("/api/v3/repos/%s/%s", owner, repo.Name),
		"author_association": "OWNER",
	}
}

// Fingerprint-cached single-issue JSON.
//
// The read workload GETs the same issue 2,000 times on one connection;
// rebuilding its map (IssueJson) and re-marshaling it (encoding/json
// sorts map keys) on every GET is the read path's biggest server-side
// cost. The TS round-3 fingerprint cache was skipped in the Go port on
// the unmeasured claim that "Go marshaling is fast enough" — the read
// workload's 473–758 ms says otherwise.
//
// Correctness: the fingerprint covers EVERY field IssueJson reads, so a
// hit can never serve a stale body — any mutation changes the
// fingerprint and the entry recomputes. Stored in a sync.Map keyed by
// the issue pointer, so concurrent GETs under the read lock are
// race-free (two goroutines computing the same value is harmless).
// Pointer reuse after GC is safe too: the fingerprint carries repo.ID
// + issue number + all fields, so a reused pointer for a different
// issue cannot match the stored fingerprint.
type issueJSONEntry struct {
	fp   string
	json string
}

var issueJSONCache sync.Map

func writeFPText(b *strings.Builder, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
	b.WriteByte(0)
}

func (s *Store) issueFingerprint(repo *Repo, issue *Issue) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(repo.ID))
	b.WriteByte(0)
	b.WriteString(s.OwnerLogin(repo))
	b.WriteByte(0)
	b.WriteString(repo.Name)
	b.WriteByte(0)
	b.WriteString(strconv.Itoa(issue.ID))
	b.WriteByte(0)
	b.WriteString(strconv.Itoa(issue.Number))
	b.WriteByte(0)
	writeFPText(&b, issue.Title)
	writeFPText(&b, issue.Body)
	b.WriteString(issue.State)
	b.WriteByte(0)
	b.WriteString(strconv.Itoa(len(issue.Comments)))
	b.WriteByte(0)
	b.WriteString(issue.CreatedAt)
	b.WriteByte(0)
	b.WriteString(issue.UpdatedAt)
	b.WriteByte(0)
	if issue.ClosedAt == nil {
		b.WriteString("nil")
	} else {
		b.WriteString(*issue.ClosedAt)
	}
	for _, l := range issue.Labels {
		b.WriteByte(0)
		b.WriteString(strconv.Itoa(l.ID))
		b.WriteByte(0)
		writeFPText(&b, l.Name)
		writeFPText(&b, l.Color)
		writeFPText(&b, l.Description)
	}
	return b.String()
}

// Serialized single-issue JSON (with trailing newline, as sendJson appends).
func (s *Store) IssueJsonString(repo *Repo, issue *Issue) string {
	fp := s.issueFingerprint(repo, issue)
	if v, ok := issueJSONCache.Load(issue); ok {
		if e, ok := v.(issueJSONEntry); ok && e.fp == fp {
			return e.json
		}
	}
	js := jsonString(s.IssueJson(repo, issue)) + "\n"
	issueJSONCache.Store(issue, issueJSONEntry{fp: fp, json: js})
	return js
}

func (s *Store) CommentJson(repo *Repo, issue *Issue, comment *Comment) map[string]any {
	owner := s.OwnerLogin(repo)
	return map[string]any{
		"id": comment.ID, "node_id": s.CommentGlobalId(repo, issue, comment),
		"body":       comment.Body,
		"user":       map[string]any{"login": owner, "id": 1000, "type": "User"},
		"created_at": comment.CreatedAt, "updated_at": comment.UpdatedAt,
		"html_url":           fmt.Sprintf("https://example.invalid/%s/%s/issues/%d#issuecomment-%d", owner, repo.Name, issue.Number, comment.ID),
		"url":                fmt.Sprintf("/api/v3/repos/%s/%s/issues/comments/%d", owner, repo.Name, comment.ID),
		"issue_url":          fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", owner, repo.Name, issue.Number),
		"author_association": "OWNER",
	}
}

func (s *Store) PullJson(repo *Repo, pr *PullRequest) map[string]any {
	owner := s.OwnerLogin(repo)
	files := s.PRFiles(repo, pr)
	additions, deletions := 0, 0
	for _, f := range files {
		additions += f.Additions
		deletions += f.Deletions
	}
	var mergeable any = nil
	var mergeableState string = "unknown"
	if pr.State == "open" {
		if s.PRMergeable(repo, pr) == "MERGEABLE" {
			mergeable = "MERGEABLE"
			mergeableState = "clean"
		} else {
			mergeable = "CONFLICTING"
			mergeableState = "dirty"
		}
	}
	return map[string]any{
		"id": pr.ID, "node_id": s.PullGlobalId(repo, pr), "number": pr.Number,
		"title": pr.Title, "body": pr.Body, "state": pr.State, "locked": false,
		"user":   map[string]any{"login": owner, "id": 1000, "type": "User"},
		"labels": []any{},
		"head": map[string]any{
			"label": owner + ":" + pr.Head, "ref": pr.Head, "sha": pr.HeadSha,
			"user": map[string]any{"login": owner, "id": 1000, "type": "User"},
			"repo": s.RepoJson(repo),
		},
		"base": map[string]any{
			"label": owner + ":" + pr.Base, "ref": pr.Base, "sha": pr.BaseSha,
			"user": map[string]any{"login": owner, "id": 1000, "type": "User"},
			"repo": s.RepoJson(repo),
		},
		"merged": pr.Merged, "mergeable": mergeable, "mergeable_state": mergeableState,
		"merge_commit_sha": pr.MergeCommitSha, "merged_at": pr.MergedAt, "merged_by": nil,
		"created_at": pr.CreatedAt, "updated_at": pr.UpdatedAt, "closed_at": pr.ClosedAt,
		"html_url":  fmt.Sprintf("https://example.invalid/%s/%s/pull/%d", owner, repo.Name, pr.Number),
		"url":       fmt.Sprintf("/api/v3/repos/%s/%s/pulls/%d", owner, repo.Name, pr.Number),
		"diff_url":  fmt.Sprintf("https://example.invalid/%s/%s/pull/%d.diff", owner, repo.Name, pr.Number),
		"patch_url": fmt.Sprintf("https://example.invalid/%s/%s/pull/%d.patch", owner, repo.Name, pr.Number),
		"additions": additions, "deletions": deletions, "changed_files": len(files),
		"commits":  s.CountPullCommits(repo, pr),
		"comments": 0, "review_comments": 0, "author_association": "OWNER",
	}
}
