package main

import (
	"encoding/base64"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func (s *Server) handleREST(w http.ResponseWriter, r *http.Request, sub string) {
	raw := strings.Trim(sub, "/")
	var segs []string
	if raw != "" {
		segs = strings.Split(raw, "/")
	}
	if len(segs) == 0 {
		if r.Method != http.MethodGet {
			writeJSON(w, 404, errBody("Not Found"))
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, user")
		writeJSON(w, 200, map[string]any{
			"current_user_url":                     "http://" + r.Host + "/api/v3/user",
			"current_user_authorizations_html_url": "https://github.com/settings/connections/applications{/client_id}",
			"authorizations_url":                   "http://" + r.Host + "/api/v3/authorizations",
			"repository_url":                       "http://" + r.Host + "/api/v3/repos/{owner}/{repo}",
			"user_repositories_url":                "http://" + r.Host + "/api/v3/users/{user}/repos{?type,page,per_page,sort}",
			"user_url":                             "http://" + r.Host + "/api/v3/users/{user}",
			"emojis_url":                           "http://" + r.Host + "/api/v3/emojis",
			"rate_limit_url":                       "http://" + r.Host + "/api/v3/rate_limit",
		})
		return
	}
	switch segs[0] {
	case "meta":
		if len(segs) == 1 && r.Method == http.MethodGet {
			writeJSON(w, 200, map[string]any{
				"installed_version":                  "forge-1.0.0",
				"verifiable_password_authentication": false,
				"hooks":                              []string{},
				"git":                                []string{},
			})
			return
		}
	case "user":
		if len(segs) == 1 && r.Method == http.MethodGet {
			s.mu.RLock()
			if !s.adopted {
				s.mu.RUnlock()
				writeJSON(w, 404, errBody("Not Found"))
				return
			}
			user := s.userJSONLocked()
			s.mu.RUnlock()
			writeJSON(w, 200, user)
			return
		}
		if len(segs) == 2 && segs[1] == "repos" {
			s.handleUserRepos(w, r)
			return
		}
	case "users":
		if len(segs) == 3 && segs[2] == "repos" && r.Method == http.MethodGet {
			s.handleUserRepos(w, r)
			return
		}
	case "repos":
		if len(segs) >= 3 {
			s.handleRepoRoute(w, r, segs[1], segs[2], segs[3:])
			return
		}
	}
	writeJSON(w, 404, errBody("Not Found"))
}

func (s *Server) handleUserRepos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		repos := s.listReposLocked()
		perPage, page := pageBounds(r.URL.Query(), 30)
		out := make([]ghRepo, 0, len(repos))
		start := (page - 1) * perPage
		if start < len(repos) {
			end := start + perPage
			if end > len(repos) {
				end = len(repos)
			}
			for _, repo := range repos[start:end] {
				out = append(out, s.repoJSONLocked(repo))
			}
		}
		hasNext := page*perPage < len(repos)
		s.mu.Unlock()
		if hasNext {
			host := r.Host
			if host == "" {
				host = "127.0.0.1"
			}
			next := "http://" + host + "/api/v3/user/repos?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1)
			w.Header().Set("Link", "<"+next+`>; rel="next"`)
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Private     bool   `json:"private"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeJSON(w, 400, errBody("Problems parsing JSON"))
			return
		}
		if body.Name == "" {
			writeJSON(w, 422, map[string]any{"message": "Repository creation failed.", "errors": []any{map[string]any{"resource": "Repository", "code": "custom", "field": "name", "message": "name is required"}}})
			return
		}
		s.mu.Lock()
		if s.repos[body.Name] != nil {
			s.mu.Unlock()
			writeJSON(w, 422, map[string]any{"message": "Repository creation failed.", "errors": []any{map[string]any{"resource": "Repository", "code": "custom", "field": "name", "message": "name already exists on this account"}}})
			return
		}
		repo := newRepo(s.nextRepoID, body.Name)
		s.nextRepoID++
		repo.Description = body.Description
		repo.Private = body.Private
		s.repos[body.Name] = repo
		out := s.repoJSONLocked(repo)
		s.mu.Unlock()
		writeJSON(w, 201, out)
	default:
		writeJSON(w, 404, errBody("Not Found"))
	}
}

func (s *Server) handleRepoRoute(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	s.mu.Lock()
	repo := s.repoLocked(owner, name)
	if repo == nil {
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			out := s.repoJSONLocked(repo)
			s.mu.Unlock()
			writeJSON(w, 200, out)
		case http.MethodPatch:
			var body struct {
				Description *string `json:"description"`
				Private     *bool   `json:"private"`
			}
			if err := decodeBody(r, &body); err != nil {
				s.mu.Unlock()
				writeJSON(w, 400, errBody("Problems parsing JSON"))
				return
			}
			if body.Description != nil {
				repo.Description = *body.Description
			}
			if body.Private != nil {
				repo.Private = *body.Private
			}
			out := s.repoJSONLocked(repo)
			s.mu.Unlock()
			writeJSON(w, 200, out)
		case http.MethodDelete:
			delete(s.repos, name)
			s.mu.Unlock()
			w.WriteHeader(204)
		default:
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
		}
		return
	}
	switch rest[0] {
	case "contents":
		path := strings.Join(rest[1:], "/")
		s.handleContents(w, r, repo, path)
	case "git":
		s.handleGitAPI(w, r, repo, rest)
	case "branches":
		s.handleBranches(w, r, repo, rest[1:])
	case "commits":
		s.handleCommits(w, r, repo)
	case "labels":
		s.handleLabels(w, r, repo)
	case "issues":
		s.handleIssues(w, r, repo, rest[1:])
	case "pulls":
		s.handlePulls(w, r, repo, rest[1:])
	default:
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
	}
}

// ---------------------------------------------------------------- contents

func (s *Server) handleContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	path = strings.Trim(path, "/")
	ref := r.URL.Query().Get("ref")
	switch r.Method {
	case http.MethodGet:
		commit, ok := peelRef(repo, ref)
		if !ok {
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
			return
		}
		tree := treeOfCommit(repo, commit)
		if path == "" {
			entries := s.listTreeLocked(repo, tree, "")
			s.mu.Unlock()
			writeJSON(w, 200, entries)
			return
		}
		entry, found := findEntry(repo, tree, path)
		if !found {
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
			return
		}
		if entry.mode == "40000" {
			entries := s.listTreeLocked(repo, entry.oid, path)
			s.mu.Unlock()
			writeJSON(w, 200, entries)
			return
		}
		obj := repo.Objects[entry.oid]
		if obj == nil {
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
			return
		}
		name := path
		if idx := strings.LastIndex(path, "/"); idx >= 0 {
			name = path[idx+1:]
		}
		out := map[string]any{
			"type":     "file",
			"encoding": "base64",
			"size":     len(obj.data),
			"name":     name,
			"path":     path,
			"sha":      entry.oid,
			"content":  base64.StdEncoding.EncodeToString(obj.data),
		}
		s.mu.Unlock()
		writeJSON(w, 200, out)
	case http.MethodPut:
		var body struct {
			Message string `json:"message"`
			Content string `json:"content"`
			Branch  string `json:"branch"`
			Sha     string `json:"sha"`
		}
		if err := decodeBody(r, &body); err != nil {
			s.mu.Unlock()
			writeJSON(w, 400, errBody("Problems parsing JSON"))
			return
		}
		if path == "" {
			s.mu.Unlock()
			writeJSON(w, 422, errBody("path is required"))
			return
		}
		data, derr := base64.StdEncoding.DecodeString(body.Content)
		if derr != nil {
			s.mu.Unlock()
			writeJSON(w, 400, errBody("content is not valid base64"))
			return
		}
		branch := strings.TrimPrefix(body.Branch, "refs/heads/")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		baseCommit, hasBase := peelRef(repo, branch)
		if !hasBase && branch != repo.DefaultBranch {
			if tip, ok := peelRef(repo, repo.DefaultBranch); ok {
				baseCommit, hasBase = tip, true
			}
		}
		baseTree := ""
		var parents []string
		if hasBase {
			baseTree = treeOfCommit(repo, baseCommit)
			parents = []string{baseCommit}
		}
		parts := strings.Split(path, "/")
		existed := false
		if entry, ok := findEntry(repo, baseTree, path); ok && entry.mode != "40000" {
			existed = true
			if body.Sha != "" && body.Sha != entry.oid {
				s.mu.Unlock()
				writeJSON(w, 409, errBody("sha does not match the file's current sha"))
				return
			}
		} else if body.Sha != "" {
			s.mu.Unlock()
			writeJSON(w, 409, errBody("sha does not match the file's current sha"))
			return
		}
		blobOID := s.storeObj(repo, "blob", data)
		newTree := s.setPath(repo, baseTree, parts, blobOID)
		msg := body.Message
		if msg == "" {
			msg = "Update " + path
		}
		commit := s.newCommit(repo, newTree, parents, msg)
		repo.Refs["refs/heads/"+branch] = commit
		repo.touchRefs()
		name := parts[len(parts)-1]
		out := map[string]any{
			"content": map[string]any{
				"type":     "file",
				"encoding": "base64",
				"size":     len(data),
				"name":     name,
				"path":     path,
				"sha":      blobOID,
				"content":  base64.StdEncoding.EncodeToString(data),
			},
			"commit": map[string]any{"sha": commit, "message": msg},
		}
		s.mu.Unlock()
		if existed {
			writeJSON(w, 200, out)
		} else {
			writeJSON(w, 201, out)
		}
	case http.MethodDelete:
		var body struct {
			Message string `json:"message"`
			Branch  string `json:"branch"`
			Sha     string `json:"sha"`
		}
		if err := decodeBody(r, &body); err != nil {
			s.mu.Unlock()
			writeJSON(w, 400, errBody("Problems parsing JSON"))
			return
		}
		if path == "" {
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
			return
		}
		branch := strings.TrimPrefix(body.Branch, "refs/heads/")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		baseCommit, hasBase := peelRef(repo, branch)
		if !hasBase {
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
			return
		}
		baseTree := treeOfCommit(repo, baseCommit)
		entry, ok := findEntry(repo, baseTree, path)
		if !ok || entry.mode == "40000" {
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
			return
		}
		if body.Sha != "" && body.Sha != entry.oid {
			s.mu.Unlock()
			writeJSON(w, 409, errBody("sha does not match the file's current sha"))
			return
		}
		parts := strings.Split(path, "/")
		newTree := s.removePath(repo, baseTree, parts)
		msg := body.Message
		if msg == "" {
			msg = "Delete " + path
		}
		commit := s.newCommit(repo, newTree, []string{baseCommit}, msg)
		repo.Refs["refs/heads/"+branch] = commit
		repo.touchRefs()
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"content": nil, "commit": map[string]any{"sha": commit, "message": msg}})
	default:
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
	}
}

func (s *Server) listTreeLocked(repo *Repo, treeOID, prefix string) []map[string]any {
	obj := repo.Objects[treeOID]
	if obj == nil || obj.typ != "tree" {
		return []map[string]any{}
	}
	entries, err := parseTree(obj.data)
	if err != nil {
		return []map[string]any{}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		kind := "file"
		size := 0
		if e.mode == "40000" {
			kind = "dir"
		} else if blob := repo.Objects[e.oid]; blob != nil {
			size = len(blob.data)
		}
		path := e.name
		if prefix != "" {
			path = prefix + "/" + e.name
		}
		out = append(out, map[string]any{
			"type": kind,
			"name": e.name,
			"path": path,
			"sha":  e.oid,
			"size": size,
			"mode": e.mode,
		})
	}
	return out
}

// ---------------------------------------------------------------- git refs and branches

func (s *Server) handleGitAPI(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) < 2 || rest[1] != "refs" {
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	sub := rest[2:]
	if len(sub) == 0 {
		switch r.Method {
		case http.MethodGet:
			out := make([]map[string]any, 0, len(repo.Refs))
			for _, name := range sortedRefNames(repo) {
				out = append(out, s.refJSONLocked(repo, name))
			}
			s.mu.Unlock()
			writeJSON(w, 200, out)
		case http.MethodPost:
			var body struct {
				Ref string `json:"ref"`
				Sha string `json:"sha"`
			}
			if err := decodeBody(r, &body); err != nil {
				s.mu.Unlock()
				writeJSON(w, 400, errBody("Problems parsing JSON"))
				return
			}
			if !strings.HasPrefix(body.Ref, "refs/") {
				s.mu.Unlock()
				writeJSON(w, 422, errBody("ref must start with refs/"))
				return
			}
			repo.Refs[body.Ref] = body.Sha
			repo.touchRefs()
			out := s.refJSONLocked(repo, body.Ref)
			s.mu.Unlock()
			writeJSON(w, 201, out)
		default:
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
		}
		return
	}
	refName := "refs/" + strings.Join(sub, "/")
	switch r.Method {
	case http.MethodGet:
		if _, ok := repo.Refs[refName]; !ok {
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
			return
		}
		out := s.refJSONLocked(repo, refName)
		s.mu.Unlock()
		writeJSON(w, 200, out)
	case http.MethodPatch:
		var body struct {
			Sha   string `json:"sha"`
			Force bool   `json:"force"`
		}
		if err := decodeBody(r, &body); err != nil {
			s.mu.Unlock()
			writeJSON(w, 400, errBody("Problems parsing JSON"))
			return
		}
		repo.Refs[refName] = body.Sha
		repo.touchRefs()
		out := s.refJSONLocked(repo, refName)
		s.mu.Unlock()
		writeJSON(w, 200, out)
	case http.MethodDelete:
		delete(repo.Refs, refName)
		repo.touchRefs()
		s.mu.Unlock()
		w.WriteHeader(204)
	default:
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
	}
}

func (s *Server) refJSONLocked(repo *Repo, name string) map[string]any {
	oid := repo.Refs[name]
	typ := "commit"
	if obj := repo.Objects[oid]; obj != nil {
		typ = obj.typ
	}
	return map[string]any{
		"ref": name,
		"object": map[string]any{
			"sha":  oid,
			"type": typ,
			"url":  "http://forge.local/api/v3/repos/" + s.viewer + "/" + repo.Name + "/git/refs/" + name,
		},
	}
}

func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if r.Method != http.MethodGet {
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	if len(rest) > 0 {
		name := strings.Join(rest, "/")
		oid, ok := repo.Refs["refs/heads/"+name]
		if !ok {
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
			return
		}
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"name":      name,
			"commit":    map[string]any{"sha": oid},
			"protected": false,
		})
		return
	}
	names := make([]string, 0)
	for name := range repo.Refs {
		if strings.HasPrefix(name, "refs/heads/") {
			names = append(names, strings.TrimPrefix(name, "refs/heads/"))
		}
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{
			"name":      name,
			"commit":    map[string]any{"sha": repo.Refs["refs/heads/"+name]},
			"protected": false,
		})
	}
	s.mu.Unlock()
	writeJSON(w, 200, out)
}

func (s *Server) handleCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	if r.Method != http.MethodGet {
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	sha := r.URL.Query().Get("sha")
	tip, ok := peelRef(repo, sha)
	if !ok {
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	oids := reachableCommits(repo, tip)
	type commitOut struct {
		sha string
		ts  int64
	}
	commits := make([]commitOut, 0, len(oids))
	for _, oid := range oids {
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		commits = append(commits, commitOut{sha: oid, ts: parseCommit(obj.data).timestamp})
	}
	sort.SliceStable(commits, func(i, j int) bool {
		if commits[i].ts != commits[j].ts {
			return commits[i].ts > commits[j].ts
		}
		return commits[i].sha > commits[j].sha
	})
	perPage, page := pageBounds(r.URL.Query(), 30)
	start := (page - 1) * perPage
	var out []map[string]any
	if start < len(commits) {
		end := start + perPage
		if end > len(commits) {
			end = len(commits)
		}
		for _, c := range commits[start:end] {
			obj := repo.Objects[c.sha]
			info := parseCommit(obj.data)
			out = append(out, map[string]any{
				"sha": c.sha,
				"commit": map[string]any{
					"message":   info.message,
					"author":    map[string]any{"name": "Forge", "email": "forge@example.invalid", "date": "2026-01-01T00:00:00Z"},
					"committer": map[string]any{"name": "Forge", "email": "forge@example.invalid", "date": "2026-01-01T00:00:00Z"},
				},
				"parents": parentShas(info.parents),
			})
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	s.mu.Unlock()
	writeJSON(w, 200, out)
}

func parentShas(parents []string) []map[string]any {
	out := make([]map[string]any, 0, len(parents))
	for _, p := range parents {
		out = append(out, map[string]any{"sha": p})
	}
	return out
}

// ---------------------------------------------------------------- labels

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request, repo *Repo) {
	switch r.Method {
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		if err := decodeBody(r, &body); err != nil {
			s.mu.Unlock()
			writeJSON(w, 400, errBody("Problems parsing JSON"))
			return
		}
		if body.Name == "" {
			s.mu.Unlock()
			writeJSON(w, 422, errBody("name is required"))
			return
		}
		if body.Color == "" {
			body.Color = "ededed"
		}
		label := repo.labelIndex[body.Name]
		if label == nil {
			label = &Label{ID: len(repo.Labels) + 1, Name: body.Name, Color: body.Color}
			repo.Labels = append(repo.Labels, label)
			repo.labelIndex[body.Name] = label
		} else {
			label.Color = body.Color
		}
		repo.touchIssues()
		out := ghLabel{ID: label.ID, Name: label.Name, Color: label.Color}
		s.mu.Unlock()
		writeJSON(w, 201, out)
	case http.MethodGet:
		out := make([]ghLabel, 0, len(repo.Labels))
		for _, l := range repo.Labels {
			out = append(out, ghLabel{ID: l.ID, Name: l.Name, Color: l.Color})
		}
		s.mu.Unlock()
		writeJSON(w, 200, out)
	default:
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
	}
}

// ---------------------------------------------------------------- issues

func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			state := r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			sortBy := r.URL.Query().Get("sort")
			direction := r.URL.Query().Get("direction")
			list := make([]*Issue, 0, len(repo.Issues))
			for _, is := range repo.Issues {
				if state != "all" && is.State != state {
					continue
				}
				list = append(list, is)
			}
			sort.SliceStable(list, func(i, j int) bool {
				var a, b int
				switch sortBy {
				case "updated":
					a, b = list[i].CreatedSeq, list[j].CreatedSeq
				default:
					a, b = list[i].CreatedSeq, list[j].CreatedSeq
				}
				if direction == "asc" {
					return a < b
				}
				return a > b
			})
			perPage, page := pageBounds(r.URL.Query(), 30)
			start := (page - 1) * perPage
			var body []byte
			body = append(body, '[')
			first := true
			if start < len(list) {
				end := start + perPage
				if end > len(list) {
					end = len(list)
				}
				for _, is := range list[start:end] {
					if !first {
						body = append(body, ',')
					}
					first = false
					body = append(body, s.issueBytesLocked(repo, is)...)
				}
			}
			body = append(body, ']')
			s.mu.Unlock()
			writeJSONBytes(w, 200, body)
		case http.MethodPost:
			var body struct {
				Title  string   `json:"title"`
				Body   string   `json:"body"`
				Labels []string `json:"labels"`
			}
			if err := decodeBody(r, &body); err != nil {
				s.mu.Unlock()
				writeJSON(w, 400, errBody("Problems parsing JSON"))
				return
			}
			for _, name := range body.Labels {
				if repo.labelIndex[name] == nil {
					s.mu.Unlock()
					writeJSON(w, 422, map[string]any{
						"message": "Validation Failed",
						"errors":  []any{map[string]any{"resource": "Issue", "code": "invalid", "field": "labels", "message": "Label does not exist"}},
					})
					return
				}
			}
			is := s.createIssueLocked(repo, body.Title, body.Body, body.Labels)
			out := s.issueJSONLocked(repo, is)
			s.mu.Unlock()
			writeJSON(w, 201, out)
		default:
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
		}
		return
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	is := repo.issueByNumberLocked(number)
	if is == nil {
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case http.MethodGet:
			body := s.issueBytesLocked(repo, is)
			s.mu.Unlock()
			writeJSONBytes(w, 200, body)
		case http.MethodPatch:
			var body struct {
				Title *string `json:"title"`
				Body  *string `json:"body"`
				State *string `json:"state"`
			}
			if err := decodeBody(r, &body); err != nil {
				s.mu.Unlock()
				writeJSON(w, 400, errBody("Problems parsing JSON"))
				return
			}
			if body.Title != nil {
				is.Title = *body.Title
			}
			if body.Body != nil {
				is.Body = *body.Body
			}
			if body.State != nil {
				switch *body.State {
				case "closed":
					if is.State != "closed" {
						now := s.stampLocked()
						is.ClosedAt = &now
					}
					is.State = "closed"
				case "open":
					is.State = "open"
					is.ClosedAt = nil
				}
			}
			is.UpdatedAt = s.stampLocked()
			repo.touchIssues()
			out := s.issueJSONLocked(repo, is)
			s.mu.Unlock()
			writeJSON(w, 200, out)
		default:
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
		}
		return
	}
	if rest[1] == "comments" {
		switch r.Method {
		case http.MethodGet:
			comments := repo.Comments[number]
			out := make([]ghComment, 0, len(comments))
			user := s.userJSONLocked()
			for _, c := range comments {
				out = append(out, ghComment{ID: c.ID, Body: c.Body, User: user, CreatedAt: c.CreatedAt, UpdatedAt: c.CreatedAt})
			}
			s.mu.Unlock()
			writeJSON(w, 200, out)
		case http.MethodPost:
			var body struct {
				Body string `json:"body"`
			}
			if err := decodeBody(r, &body); err != nil {
				s.mu.Unlock()
				writeJSON(w, 400, errBody("Problems parsing JSON"))
				return
			}
			now := s.stampLocked()
			c := &Comment{ID: s.nextCommentID, Body: body.Body, CreatedAt: now}
			s.nextCommentID++
			repo.Comments[number] = append(repo.Comments[number], c)
			repo.touchIssues()
			out := ghComment{ID: c.ID, Body: c.Body, User: s.userJSONLocked(), CreatedAt: c.CreatedAt, UpdatedAt: c.CreatedAt}
			s.mu.Unlock()
			writeJSON(w, 201, out)
		default:
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
		}
		return
	}
	s.mu.Unlock()
	writeJSON(w, 404, errBody("Not Found"))
}

// ---------------------------------------------------------------- pulls

func splitHeadRef(head string) string {
	if idx := strings.LastIndex(head, ":"); idx >= 0 {
		return head[idx+1:]
	}
	return head
}

func (s *Server) handlePulls(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodPost:
			var body struct {
				Title string `json:"title"`
				Body  string `json:"body"`
				Head  string `json:"head"`
				Base  string `json:"base"`
			}
			if err := decodeBody(r, &body); err != nil {
				s.mu.Unlock()
				writeJSON(w, 400, errBody("Problems parsing JSON"))
				return
			}
			headRef := splitHeadRef(body.Head)
			baseRef := splitHeadRef(body.Base)
			headSha, okHead := peelRef(repo, headRef)
			baseSha, okBase := peelRef(repo, baseRef)
			if !okHead || !okBase {
				s.mu.Unlock()
				writeJSON(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "PullRequest", "code": "custom", "message": "field is invalid"}}})
				return
			}
			pr := s.createPullLocked(repo, body.Title, body.Body, headRef, headSha, baseRef, baseSha)
			out := s.pullJSONLocked(repo, pr)
			s.mu.Unlock()
			writeJSON(w, 201, out)
		case http.MethodGet:
			state := r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			perPage, page := pageBounds(r.URL.Query(), 30)
			list := make([]*Pull, 0, len(repo.Pulls))
			for _, pr := range repo.Pulls {
				if state != "all" && pr.State != state {
					continue
				}
				list = append(list, pr)
			}
			sort.SliceStable(list, func(i, j int) bool { return list[i].Number > list[j].Number })
			start := (page - 1) * perPage
			out := make([]ghPull, 0, len(list))
			if start < len(list) {
				end := start + perPage
				if end > len(list) {
					end = len(list)
				}
				for _, pr := range list[start:end] {
					out = append(out, s.pullJSONLocked(repo, pr))
				}
			}
			s.mu.Unlock()
			writeJSON(w, 200, out)
		default:
			s.mu.Unlock()
			writeJSON(w, 404, errBody("Not Found"))
		}
		return
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	pr := repo.pullByNumberLocked(number)
	if pr == nil {
		s.mu.Unlock()
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	if len(rest) == 1 && r.Method == http.MethodGet {
		out := s.pullJSONLocked(repo, pr)
		s.mu.Unlock()
		writeJSON(w, 200, out)
		return
	}
	if len(rest) == 2 && rest[1] == "files" && r.Method == http.MethodGet {
		out := s.pullFilesLocked(repo, pr)
		s.mu.Unlock()
		writeJSON(w, 200, out)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && (r.Method == http.MethodPut || r.Method == http.MethodPost) {
		out, status := s.mergePullLocked(repo, pr)
		s.mu.Unlock()
		writeJSON(w, status, out)
		return
	}
	s.mu.Unlock()
	writeJSON(w, 404, errBody("Not Found"))
}

func (s *Server) pullFilesLocked(repo *Repo, pr *Pull) []map[string]any {
	headTree := treeOfCommit(repo, pr.HeadSha)
	baseCommit, ok := peelRef(repo, pr.BaseRef)
	if !ok {
		baseCommit = pr.BaseSha
	}
	baseTree := treeOfCommit(repo, baseCommit)
	mb := mergeBaseOf(repo, baseCommit, pr.HeadSha)
	compareTree := baseTree
	if mb != "" {
		if t := treeOfCommit(repo, mb); t != "" {
			compareTree = t
		}
	}
	paths := diffTreePaths(repo, compareTree, headTree)
	out := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		status := "modified"
		if _, ok := findEntry(repo, compareTree, path); !ok {
			status = "added"
		}
		if _, ok := findEntry(repo, headTree, path); !ok {
			status = "removed"
		}
		additions := 1
		deletions := 0
		out = append(out, map[string]any{
			"filename":  path,
			"status":    status,
			"additions": additions,
			"deletions": deletions,
			"changes":   additions + deletions,
		})
	}
	return out
}

func (s *Server) mergePullLocked(repo *Repo, pr *Pull) (map[string]any, int) {
	fail := func(status int, msg string) (map[string]any, int) {
		return errBody(msg), status
	}
	if pr.State == "closed" || pr.Merged {
		return fail(405, "Pull Request is not mergeable")
	}
	baseTip, ok := peelRef(repo, pr.BaseRef)
	if !ok || baseTip == "" {
		baseTip = pr.BaseSha
	}
	headCommit := pr.HeadSha
	if baseTip == "" || headCommit == "" {
		return fail(405, "Pull Request is not mergeable")
	}
	baseTree := treeOfCommit(repo, baseTip)
	headTree := treeOfCommit(repo, headCommit)
	if headTree == "" {
		return fail(405, "Pull Request is not mergeable")
	}
	mb := mergeBaseOf(repo, baseTip, headCommit)
	if mb != "" && mb != baseTip && mb != headCommit {
		mbTree := treeOfCommit(repo, mb)
		changedBase := diffTreePaths(repo, mbTree, baseTree)
		changedHead := diffTreePaths(repo, mbTree, headTree)
		baseSet := make(map[string]bool, len(changedBase))
		for _, p := range changedBase {
			baseSet[p] = true
		}
		for _, p := range changedHead {
			if baseSet[p] {
				return fail(405, "Pull Request is not mergeable")
			}
		}
	}
	msg := pr.Title
	if msg == "" {
		msg = "Merge pull request #" + strconv.Itoa(pr.Number)
	}
	var parents []string
	if baseTip != "" {
		parents = []string{baseTip}
	}
	commit := s.newCommit(repo, headTree, parents, msg)
	repo.Refs["refs/heads/"+pr.BaseRef] = commit
	repo.touchRefs()
	now := s.stampLocked()
	pr.Merged = true
	pr.State = "closed"
	pr.MergedAt = &now
	pr.MergeSHA = &commit
	pr.UpdatedAt = now
	return map[string]any{"sha": commit, "merged": true, "message": "Pull Request successfully merged"}, 200
}
