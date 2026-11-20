package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// gObj is one stored git object.
type gObj struct {
	typ  string
	data []byte
}

// Label is a repository label.
type Label struct {
	ID    int
	Name  string
	Color string
}

// Issue is one issue; Number is 1-based and stable for the repository's life.
type Issue struct {
	ID           int
	Number       int
	Title        string
	Body         string
	State        string
	Labels       []string
	CreatedAt    string
	UpdatedAt    string
	ClosedAt     *string
	CreatedSeq   int
	CommentCount int
	cache        []byte
	cacheGen     int64
}

// Comment is one issue comment.
type Comment struct {
	ID        int
	Body      string
	CreatedAt string
}

// Pull is one pull request.
type Pull struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string
	HeadRef    string
	HeadSha    string
	BaseRef    string
	BaseSha    string
	Merged     bool
	MergedAt   *string
	MergeSHA   *string
	CreatedAt  string
	UpdatedAt  string
	CreatedSeq int
}

// Repo is one repository with its objects, refs and API state.
type Repo struct {
	ID            int
	Name          string
	Description   string
	Private       bool
	DefaultBranch string

	Objects map[string]*gObj
	Refs    map[string]string

	Issues     []*Issue
	Pulls      []*Pull
	Labels     []*Label
	labelIndex map[string]*Label
	Comments   map[int][]*Comment
	NextPull   int

	// apiGen advances on every issue-side mutation; issue JSON cached under an older value is
	// rebuilt on its next read.
	apiGen int64
	// refGen advances on every ref mutation; each cached advertisement names its generation.
	refGen  int64
	advV2   []byte
	advV0   []byte
	advRcv  []byte
	advV2G  int64
	advV0G  int64
	advRcvG int64

	packMu    sync.RWMutex
	packGen   int64
	packCache map[string]*fetchPack
}

func newRepo(id int, name string) *Repo {
	return &Repo{
		ID:            id,
		Name:          name,
		DefaultBranch: "main",
		Objects:       make(map[string]*gObj),
		Refs:          make(map[string]string),
		labelIndex:    make(map[string]*Label),
		Comments:      make(map[int][]*Comment),
		NextPull:      1,
		packCache:     make(map[string]*fetchPack),
	}
}

// touchIssues invalidates every cached issue rendering of the repository.
func (repo *Repo) touchIssues() { repo.apiGen++ }

// touchRefs invalidates the cached ref advertisements.
func (repo *Repo) touchRefs() { repo.refGen++ }

// dropPackCache forgets every cached fetch pack; called after a push changes the object store.
func (repo *Repo) dropPackCache() {
	repo.packMu.Lock()
	repo.packGen++
	repo.packCache = make(map[string]*fetchPack)
	repo.packMu.Unlock()
}

func (s *Server) adoptLocked(owner string) {
	if !s.adopted {
		s.adopted = true
		s.viewer = owner
	}
}

// repoLocked resolves a repository under the single adopted owner. Callers hold s.mu.
func (s *Server) repoLocked(owner, name string) *Repo {
	s.adoptLocked(owner)
	if owner != s.viewer {
		return nil
	}
	return s.repos[name]
}

func (s *Server) repoByIDLocked(id int) *Repo {
	for _, repo := range s.repos {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

func (s *Server) listReposLocked() []*Repo {
	out := make([]*Repo, 0, len(s.repos))
	for _, repo := range s.repos {
		out = append(out, repo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) stampLocked() string {
	s.clock++
	return time.Unix(1760000000+s.clock, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// ---------------------------------------------------------------- REST JSON shapes

type ghUser struct {
	Login string `json:"login"`
	ID    int    `json:"id,omitempty"`
	Type  string `json:"type,omitempty"`
}

type ghLabel struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type ghRepo struct {
	ID              int    `json:"id"`
	NodeID          string `json:"node_id"`
	Name            string `json:"name"`
	FullName        string `json:"full_name"`
	Owner           ghUser `json:"owner"`
	Private         bool   `json:"private"`
	Description     string `json:"description"`
	Fork            bool   `json:"fork"`
	DefaultBranch   string `json:"default_branch"`
	HasIssues       bool   `json:"has_issues"`
	HasProjects     bool   `json:"has_projects"`
	HasWiki         bool   `json:"has_wiki"`
	OpenIssuesCount int    `json:"open_issues_count"`
	Visibility      string `json:"visibility"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

type ghIssue struct {
	ID        int       `json:"id"`
	NodeID    string    `json:"node_id"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Labels    []ghLabel `json:"labels"`
	User      ghUser    `json:"user"`
	Comments  int       `json:"comments"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
	ClosedAt  *string   `json:"closed_at"`
	HTMLURL   string    `json:"html_url"`
	URL       string    `json:"url"`
}

type ghComment struct {
	ID        int    `json:"id"`
	NodeID    string `json:"node_id"`
	Body      string `json:"body"`
	User      ghUser `json:"user"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type ghRef struct {
	Ref   string `json:"ref"`
	Sha   string `json:"sha"`
	Label string `json:"label"`
}

type ghPull struct {
	ID             int     `json:"id"`
	NodeID         string  `json:"node_id"`
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	State          string  `json:"state"`
	Head           ghRef   `json:"head"`
	Base           ghRef   `json:"base"`
	Merged         bool    `json:"merged"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSha *string `json:"merge_commit_sha"`
	User           ghUser  `json:"user"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	HTMLURL        string  `json:"html_url"`
}

func (s *Server) userJSONLocked() ghUser {
	return ghUser{Login: s.viewer, ID: 1, Type: "User"}
}

func (s *Server) repoJSONLocked(repo *Repo) ghRepo {
	owner := ghUser{Login: s.viewer, ID: 1, Type: "User"}
	open := 0
	for _, is := range repo.Issues {
		if is.State == "open" {
			open++
		}
	}
	return ghRepo{
		ID:              repo.ID,
		NodeID:          "R_" + strconv.Itoa(repo.ID),
		Name:            repo.Name,
		FullName:        s.viewer + "/" + repo.Name,
		Owner:           owner,
		Private:         repo.Private,
		Description:     repo.Description,
		DefaultBranch:   repo.DefaultBranch,
		HasIssues:       true,
		HasProjects:     true,
		HasWiki:         false,
		OpenIssuesCount: open,
		Visibility:      visibilityOf(repo.Private),
		CreatedAt:       "2026-01-01T00:00:00Z",
		UpdatedAt:       "2026-01-01T00:00:00Z",
	}
}

func visibilityOf(private bool) string {
	if private {
		return "private"
	}
	return "public"
}

func (s *Server) labelsJSONLocked(repo *Repo, names []string) []ghLabel {
	out := make([]ghLabel, 0, len(names))
	for _, name := range names {
		if label := repo.labelIndex[name]; label != nil {
			out = append(out, ghLabel{ID: label.ID, Name: label.Name, Color: label.Color})
		} else {
			out = append(out, ghLabel{Name: name, Color: "ededed"})
		}
	}
	return out
}

func (s *Server) issueJSONLocked(repo *Repo, is *Issue) ghIssue {
	return ghIssue{
		ID:        is.ID,
		NodeID:    "I_" + strconv.Itoa(is.ID),
		Number:    is.Number,
		Title:     is.Title,
		Body:      is.Body,
		State:     is.State,
		Labels:    s.labelsJSONLocked(repo, is.Labels),
		User:      s.userJSONLocked(),
		Comments:  len(repo.Comments[is.Number]),
		CreatedAt: is.CreatedAt,
		UpdatedAt: is.UpdatedAt,
		ClosedAt:  is.ClosedAt,
		HTMLURL:   "http://forge.local/" + s.viewer + "/" + repo.Name + "/issues/" + strconv.Itoa(is.Number),
		URL:       "http://forge.local/api/v3/repos/" + s.viewer + "/" + repo.Name + "/issues/" + strconv.Itoa(is.Number),
	}
}

// issueBytesLocked is issueJSONLocked rendered once per repository generation: the read benchmark
// asks for the same issue two thousand times, so the marshaled bytes are kept and only rebuilt
// after a mutation.
func (s *Server) issueBytesLocked(repo *Repo, is *Issue) []byte {
	if is.cache != nil && is.cacheGen == repo.apiGen {
		return is.cache
	}
	b, err := json.Marshal(s.issueJSONLocked(repo, is))
	if err != nil {
		return []byte(`{"message":"internal error"}`)
	}
	is.cache = b
	is.cacheGen = repo.apiGen
	return b
}

func (s *Server) pullJSONLocked(repo *Repo, pr *Pull) ghPull {
	return ghPull{
		ID:             pr.ID,
		NodeID:         "P_" + strconv.Itoa(pr.ID),
		Number:         pr.Number,
		Title:          pr.Title,
		Body:           pr.Body,
		State:          pr.State,
		Head:           ghRef{Ref: pr.HeadRef, Sha: pr.HeadSha, Label: s.viewer + ":" + pr.HeadRef},
		Base:           ghRef{Ref: pr.BaseRef, Sha: pr.BaseSha, Label: s.viewer + ":" + pr.BaseRef},
		Merged:         pr.Merged,
		MergedAt:       pr.MergedAt,
		MergeCommitSha: pr.MergeSHA,
		User:           s.userJSONLocked(),
		CreatedAt:      pr.CreatedAt,
		UpdatedAt:      pr.UpdatedAt,
		HTMLURL:        "http://forge.local/" + s.viewer + "/" + repo.Name + "/pull/" + strconv.Itoa(pr.Number),
	}
}

func (s *Server) createIssueLocked(repo *Repo, title, body string, labels []string) *Issue {
	now := s.stampLocked()
	is := &Issue{
		ID:         s.nextIssueID,
		Number:     len(repo.Issues) + 1,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     append([]string(nil), labels...),
		CreatedAt:  now,
		UpdatedAt:  now,
		CreatedSeq: len(repo.Issues) + 1,
	}
	s.nextIssueID++
	repo.Issues = append(repo.Issues, is)
	repo.touchIssues()
	return is
}

func (s *Server) createPullLocked(repo *Repo, title, body, headRef, headSha, baseRef, baseSha string) *Pull {
	now := s.stampLocked()
	pr := &Pull{
		ID:         s.nextPullID,
		Number:     repo.NextPull,
		Title:      title,
		Body:       body,
		State:      "open",
		HeadRef:    headRef,
		HeadSha:    headSha,
		BaseRef:    baseRef,
		BaseSha:    baseSha,
		CreatedAt:  now,
		UpdatedAt:  now,
		CreatedSeq: len(repo.Pulls) + 1,
	}
	s.nextPullID++
	repo.NextPull++
	repo.Pulls = append(repo.Pulls, pr)
	return pr
}

func (repo *Repo) issueByNumberLocked(number int) *Issue {
	if number < 1 || number > len(repo.Issues) {
		return nil
	}
	return repo.Issues[number-1]
}

func (repo *Repo) pullByNumberLocked(number int) *Pull {
	for _, pr := range repo.Pulls {
		if pr.Number == number {
			return pr
		}
	}
	return nil
}

// globalID spells an opaque, stable id for a GraphQL node.
func globalID(kind string, id int) string {
	return base64.StdEncoding.EncodeToString([]byte("forge:" + kind + ":" + strconv.Itoa(id)))
}

func parseGlobalID(value string) (string, int, bool) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", 0, false
	}
	parts := splitColon(string(raw))
	if len(parts) != 3 || parts[0] != "forge" {
		return "", 0, false
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", 0, false
	}
	return parts[1], n, true
}

func splitColon(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func (s *Server) findIssueByIDLocked(id int) (*Repo, *Issue) {
	for _, repo := range s.repos {
		for _, is := range repo.Issues {
			if is.ID == id {
				return repo, is
			}
		}
	}
	return nil, nil
}

func (s *Server) findPullByIDLocked(id int) (*Repo, *Pull) {
	for _, repo := range s.repos {
		for _, pr := range repo.Pulls {
			if pr.ID == id {
				return repo, pr
			}
		}
	}
	return nil, nil
}

func pageBounds(query map[string][]string, defaultPerPage int) (perPage, page int) {
	perPage = defaultPerPage
	page = 1
	if vs := query["per_page"]; len(vs) > 0 {
		if n, err := strconv.Atoi(vs[0]); err == nil && n > 0 {
			perPage = n
		}
	}
	if perPage > 100 {
		perPage = 100
	}
	if vs := query["page"]; len(vs) > 0 {
		if n, err := strconv.Atoi(vs[0]); err == nil && n > 0 {
			page = n
		}
	}
	return
}

func intFromAny(v any, fallback int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(t); err == nil {
			return n
		}
	}
	return fallback
}

func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolFromAny(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

var _ = fmt.Sprintf
