package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const zeroOID = "0000000000000000000000000000000000000000"

// objOID computes the git object id of a typed object.
func objOID(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func isHexOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

type treeEnt struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) ([]treeEnt, error) {
	var out []treeEnt
	for i := 0; i < len(data); {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp <= 0 {
			return nil, fmt.Errorf("bad tree entry")
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := bytes.IndexByte(data[i:], 0)
		if nul < 0 {
			return nil, fmt.Errorf("bad tree name")
		}
		name := string(data[i : i+nul])
		i += nul + 1
		if i+20 > len(data) {
			return nil, fmt.Errorf("bad tree oid")
		}
		out = append(out, treeEnt{mode: mode, name: name, oid: hex.EncodeToString(data[i : i+20])})
		i += 20
	}
	return out, nil
}

func (s *Server) storeObj(repo *Repo, typ string, data []byte) string {
	oid := objOID(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = &gObj{typ: typ, data: data}
	}
	return oid
}

func treeSortKey(e treeEnt) string {
	if e.mode == "40000" {
		return e.name + "/"
	}
	return e.name
}

func (s *Server) buildTree(repo *Repo, entries []treeEnt) string {
	sorted := append([]treeEnt(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return treeSortKey(sorted[i]) < treeSortKey(sorted[j]) })
	var b bytes.Buffer
	for _, e := range sorted {
		b.WriteString(e.mode)
		b.WriteByte(' ')
		b.WriteString(e.name)
		b.WriteByte(0)
		raw, err := hex.DecodeString(e.oid)
		if err != nil {
			continue
		}
		b.Write(raw)
	}
	return s.storeObj(repo, "tree", b.Bytes())
}

type commitInfo struct {
	tree      string
	parents   []string
	author    string
	committer string
	message   string
	timestamp int64
}

func parseCommit(data []byte) commitInfo {
	var c commitInfo
	sep := bytes.Index(data, []byte("\n\n"))
	headers := data
	body := []byte(nil)
	if sep >= 0 {
		headers = data[:sep]
		body = data[sep+2:]
	}
	c.message = strings.TrimSuffix(string(body), "\n")
	for _, line := range strings.Split(string(headers), "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			c.parents = append(c.parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			c.author = line[7:]
		case strings.HasPrefix(line, "committer "):
			c.committer = line[10:]
		}
	}
	if c.committer != "" {
		fields := strings.Fields(c.committer)
		if len(fields) >= 2 {
			if n, err := strconv.ParseInt(fields[len(fields)-2], 10, 64); err == nil {
				c.timestamp = n
			}
		}
	}
	return c
}

func parseTag(data []byte) (target, typ string) {
	sep := bytes.Index(data, []byte("\n\n"))
	headers := data
	if sep >= 0 {
		headers = data[:sep]
	}
	for _, line := range strings.Split(string(headers), "\n") {
		if strings.HasPrefix(line, "object ") {
			target = strings.TrimSpace(line[7:])
		} else if strings.HasPrefix(line, "type ") {
			typ = strings.TrimSpace(line[5:])
		}
	}
	return
}

// peelToCommit follows annotated tags until a commit object, returning its oid.
func peelToCommit(repo *Repo, oid string) (string, bool) {
	for depth := 0; depth < 10; depth++ {
		obj := repo.Objects[oid]
		if obj == nil {
			return "", false
		}
		if obj.typ == "commit" {
			return oid, true
		}
		if obj.typ != "tag" {
			return "", false
		}
		target, _ := parseTag(obj.data)
		if target == "" {
			return "", false
		}
		oid = target
	}
	return "", false
}

// peelRef resolves a ref, tag or raw oid to a commit oid.
func peelRef(repo *Repo, name string) (string, bool) {
	oid, ok := refOID(repo, name)
	if !ok {
		return "", false
	}
	return peelToCommit(repo, oid)
}

func refOID(repo *Repo, name string) (string, bool) {
	if name == "" {
		name = repo.DefaultBranch
	}
	if oid, ok := repo.Refs[name]; ok {
		return oid, true
	}
	if !strings.HasPrefix(name, "refs/") {
		if oid, ok := repo.Refs["refs/heads/"+name]; ok {
			return oid, true
		}
		if oid, ok := repo.Refs["refs/tags/"+name]; ok {
			return oid, true
		}
		if isHexOID(name) {
			if _, ok := repo.Objects[name]; ok {
				return name, true
			}
		}
	}
	return "", false
}

func (s *Server) newCommit(repo *Repo, tree string, parents []string, message string) string {
	s.clock++
	ts := int64(1760000000 + s.clock)
	var b bytes.Buffer
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		if p != "" {
			b.WriteString("parent " + p + "\n")
		}
	}
	ident := "Forge <forge@example.invalid>"
	fmt.Fprintf(&b, "author %s %d +0000\n", ident, ts)
	fmt.Fprintf(&b, "committer %s %d +0000\n", ident, ts)
	b.WriteByte('\n')
	b.WriteString(message)
	return s.storeObj(repo, "commit", b.Bytes())
}

// treeOfCommit returns the root tree oid of a commit.
func treeOfCommit(repo *Repo, commitOID string) string {
	obj := repo.Objects[commitOID]
	if obj == nil || obj.typ != "commit" {
		return ""
	}
	return parseCommit(obj.data).tree
}

// walkTree walks a tree and its subtrees, calling visit for every entry with its full path.
func walkTree(repo *Repo, treeOID, prefix string, visit func(path string, e treeEnt)) {
	obj := repo.Objects[treeOID]
	if obj == nil || obj.typ != "tree" {
		return
	}
	entries, err := parseTree(obj.data)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := prefix + e.name
		visit(path, e)
		if e.mode == "40000" {
			walkTree(repo, e.oid, path+"/", visit)
		}
	}
}

// findEntry locates a path in a root tree.
func findEntry(repo *Repo, treeOID, path string) (treeEnt, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	current := treeOID
	var found treeEnt
	for i, part := range parts {
		obj := repo.Objects[current]
		if obj == nil || obj.typ != "tree" {
			return treeEnt{}, false
		}
		entries, err := parseTree(obj.data)
		if err != nil {
			return treeEnt{}, false
		}
		ok := false
		for _, e := range entries {
			if e.name == part {
				found = e
				current = e.oid
				ok = true
				break
			}
		}
		if !ok {
			return treeEnt{}, false
		}
		if i < len(parts)-1 && found.mode != "40000" {
			return treeEnt{}, false
		}
	}
	return found, true
}

// modifyTree returns a new root tree with one file set (or removed when blobOID is empty).
func (s *Server) modifyTree(repo *Repo, rootTree string, path []string, blobOID string, remove bool) string {
	if len(path) == 0 {
		return rootTree
	}
	var entries []treeEnt
	if rootTree != "" {
		if obj := repo.Objects[rootTree]; obj != nil && obj.typ == "tree" {
			if parsed, err := parseTree(obj.data); err == nil {
				entries = parsed
			}
		}
	}
	name := path[0]
	out := make([]treeEnt, 0, len(entries)+1)
	if len(path) == 1 {
		for _, e := range entries {
			if e.name != name {
				out = append(out, e)
			}
		}
		if !remove {
			out = append(out, treeEnt{mode: "100644", name: name, oid: blobOID})
		}
		return s.buildTree(repo, out)
	}
	found := false
	for _, e := range entries {
		if e.name == name {
			found = true
			child := ""
			if e.mode == "40000" {
				child = e.oid
			}
			newChild := s.modifyTree(repo, child, path[1:], blobOID, remove)
			if newChild != "" {
				out = append(out, treeEnt{mode: "40000", name: name, oid: newChild})
			}
		} else {
			out = append(out, e)
		}
	}
	if !found && !remove {
		newChild := s.modifyTree(repo, "", path[1:], blobOID, remove)
		if newChild != "" {
			out = append(out, treeEnt{mode: "40000", name: name, oid: newChild})
		}
	}
	return s.buildTree(repo, out)
}

// removePath deletes one file from a tree.
func (s *Server) removePath(repo *Repo, rootTree string, path []string) string {
	return s.modifyTree(repo, rootTree, path, "", true)
}

// setPath adds or replaces one file in a tree.
func (s *Server) setPath(repo *Repo, rootTree string, path []string, blobOID string) string {
	return s.modifyTree(repo, rootTree, path, blobOID, false)
}

// closureOf collects every object reachable from roots.
func closureOf(repo *Repo, roots []string) map[string]bool {
	seen := make(map[string]bool)
	var visit func(oid string)
	visit = func(oid string) {
		if oid == "" || seen[oid] {
			return
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil {
			return
		}
		switch obj.typ {
		case "commit":
			c := parseCommit(obj.data)
			visit(c.tree)
			for _, p := range c.parents {
				visit(p)
			}
		case "tree":
			entries, err := parseTree(obj.data)
			if err != nil {
				return
			}
			for _, e := range entries {
				visit(e.oid)
			}
		case "tag":
			target, _ := parseTag(obj.data)
			visit(target)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	return seen
}

// treeClosureOnly collects the tree/blob closure of a commit's tree, without history.
func treeClosureOf(repo *Repo, treeOID string, into map[string]bool) {
	for oid := range closureOf(repo, []string{treeOID}) {
		into[oid] = true
	}
}

// closureOfShallow is closureOf with a shallow boundary: a commit named in `shallow` contributes
// its own tree but none of its parents, so a fetch starting from a shallow tip does not treat the
// cut-off history as held.
func closureOfShallow(repo *Repo, roots, shallow []string) map[string]bool {
	boundary := make(map[string]bool, len(shallow))
	for _, oid := range shallow {
		boundary[oid] = true
	}
	seen := make(map[string]bool)
	var visit func(oid string)
	visit = func(oid string) {
		if oid == "" || seen[oid] {
			return
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil {
			return
		}
		switch obj.typ {
		case "commit":
			c := parseCommit(obj.data)
			visit(c.tree)
			if !boundary[oid] {
				for _, p := range c.parents {
					visit(p)
				}
			}
		case "tree":
			entries, err := parseTree(obj.data)
			if err != nil {
				return
			}
			for _, e := range entries {
				visit(e.oid)
			}
		case "tag":
			target, _ := parseTag(obj.data)
			visit(target)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	return seen
}

// wantClosure is the object set a fetch of wants at a given depth must deliver. It also returns
// the shallow boundary commits (commits whose parents are cut off by the depth limit).
func wantClosure(repo *Repo, wants []string, depth int) (map[string]bool, []string) {
	if depth <= 0 {
		return closureOf(repo, wants), nil
	}
	seen := make(map[string]bool)
	boundary := make(map[string]bool)
	type commitAt struct {
		oid string
		gen int
	}
	var queue []commitAt
	enqueue := func(root string) {
		oid := root
		for i := 0; i < 10; i++ {
			obj := repo.Objects[oid]
			if obj == nil {
				return
			}
			if obj.typ == "commit" {
				queue = append(queue, commitAt{oid: oid, gen: 1})
				return
			}
			if obj.typ != "tag" {
				return
			}
			seen[oid] = true
			target, _ := parseTag(obj.data)
			if target == "" {
				return
			}
			oid = target
		}
	}
	for _, w := range wants {
		enqueue(w)
	}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if seen[item.oid] {
			continue
		}
		seen[item.oid] = true
		obj := repo.Objects[item.oid]
		if obj == nil || obj.typ != "commit" {
			continue
		}
		c := parseCommit(obj.data)
		treeClosureOf(repo, c.tree, seen)
		if item.gen < depth {
			for _, p := range c.parents {
				queue = append(queue, commitAt{oid: p, gen: item.gen + 1})
			}
		} else if len(c.parents) > 0 {
			boundary[item.oid] = true
		}
	}
	bounds := make([]string, 0, len(boundary))
	for oid := range boundary {
		bounds = append(bounds, oid)
	}
	sort.Strings(bounds)
	return seen, bounds
}

// reachableCommits lists every commit reachable from a tip.
func reachableCommits(repo *Repo, tip string) []string {
	seen := make(map[string]bool)
	var out []string
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == "" || seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.typ != "commit" {
			continue
		}
		out = append(out, oid)
		c := parseCommit(obj.data)
		queue = append(queue, c.parents...)
	}
	return out
}

// isAncestor reports whether ancestor is reachable from descendant.
func isAncestor(repo *Repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{descendant: true}
	queue := []string{descendant}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		obj := repo.Objects[oid]
		if obj == nil || obj.typ != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.data).parents {
			if p == ancestor {
				return true
			}
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return false
}

// mergeBaseOf finds the best common ancestor of two commits (breadth-first from both sides).
func mergeBaseOf(repo *Repo, a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	anc := make(map[string]bool)
	queue := []string{a}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == "" || anc[oid] {
			continue
		}
		anc[oid] = true
		if obj := repo.Objects[oid]; obj != nil && obj.typ == "commit" {
			queue = append(queue, parseCommit(obj.data).parents...)
		}
	}
	queue = []string{b}
	seenB := make(map[string]bool)
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == "" || seenB[oid] {
			continue
		}
		if anc[oid] {
			return oid
		}
		seenB[oid] = true
		if obj := repo.Objects[oid]; obj != nil && obj.typ == "commit" {
			queue = append(queue, parseCommit(obj.data).parents...)
		}
	}
	return ""
}

// diffTreePaths lists paths whose content differs between two root trees.
func diffTreePaths(repo *Repo, a, b string) []string {
	var out []string
	var walk func(a, b, prefix string)
	walk = func(a, b, prefix string) {
		if a == b {
			return
		}
		ea := map[string]treeEnt{}
		eb := map[string]treeEnt{}
		if obj := repo.Objects[a]; obj != nil && obj.typ == "tree" {
			if entries, err := parseTree(obj.data); err == nil {
				for _, e := range entries {
					ea[e.name] = e
				}
			}
		}
		if obj := repo.Objects[b]; obj != nil && obj.typ == "tree" {
			if entries, err := parseTree(obj.data); err == nil {
				for _, e := range entries {
					eb[e.name] = e
				}
			}
		}
		names := make([]string, 0, len(ea)+len(eb))
		seen := map[string]bool{}
		for name := range ea {
			seen[name] = true
			names = append(names, name)
		}
		for name := range eb {
			if !seen[name] {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			x, okA := ea[name]
			y, okB := eb[name]
			path := prefix + name
			switch {
			case !okA:
				if y.mode == "40000" {
					walk("", y.oid, path+"/")
				} else {
					out = append(out, path)
				}
			case !okB:
				if x.mode == "40000" {
					walk(x.oid, "", path+"/")
				} else {
					out = append(out, path)
				}
			case x.oid == y.oid:
				continue
			case x.mode == "40000" && y.mode == "40000":
				walk(x.oid, y.oid, path+"/")
			default:
				out = append(out, path)
			}
		}
	}
	walk(a, b, "")
	return out
}
