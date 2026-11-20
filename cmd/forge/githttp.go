package main

import (
	"bytes"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const uploadAgent = "agent=forge/1.0"

func parseGitPath(path string) (owner, repo, suffix string, ok bool) {
	p := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(p, "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	if !strings.HasSuffix(parts[1], ".git") {
		return "", "", "", false
	}
	owner = parts[0]
	repo = strings.TrimSuffix(parts[1], ".git")
	suffix = parts[2]
	switch suffix {
	case "info/refs", "git-upload-pack", "git-receive-pack":
		return owner, repo, suffix, true
	}
	return "", "", "", false
}

func (s *Server) handleGit(w http.ResponseWriter, r *http.Request, owner, name, suffix string) {
	switch {
	case suffix == "info/refs" && r.Method == http.MethodGet:
		s.gitInfoRefs(w, r, owner, name)
	case suffix == "git-upload-pack" && r.Method == http.MethodPost:
		s.gitUploadPack(w, r, owner, name)
	case suffix == "git-receive-pack" && r.Method == http.MethodPost:
		s.gitReceivePack(w, r, owner, name)
	default:
		writeJSON(w, 404, errBody("Not Found"))
	}
}

// gitRepo fetches a repository for a git request, adopting the owner from the path.
func (s *Server) gitRepo(owner, name string) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repoLocked(owner, name)
}

func (s *Server) gitInfoRefs(w http.ResponseWriter, r *http.Request, owner, name string) {
	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	repo := s.gitRepo(owner, name)
	if repo == nil {
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v2 := service == "git-upload-pack" && strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
	var buf bytes.Buffer
	if service == "git-upload-pack" {
		if v2 {
			writePkt(&buf, "version 2\n")
			writePkt(&buf, "agent=forge/1.0\n")
			writePkt(&buf, "ls-refs=unborn\n")
			writePkt(&buf, "fetch=shallow filter\n")
			writePkt(&buf, "object-format=sha1\n")
			writePkt(&buf, "server-option\n")
			writeFlush(&buf)
		} else {
			writePkt(&buf, "# service=git-upload-pack\n")
			writeFlush(&buf)
			s.writeV0RefsLocked(&buf, repo, "multi_ack_detailed side-band-64k thin-pack ofs-delta "+uploadAgent)
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	} else {
		writePkt(&buf, "# service=git-receive-pack\n")
		writeFlush(&buf)
		s.writeV0RefsLocked(&buf, repo, "report-status delete-refs side-band-64k ofs-delta "+uploadAgent)
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	w.Write(buf.Bytes())
}

// headOID returns the oid HEAD names for ref advertisement.
func headOID(repo *Repo) (string, bool) {
	if oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
		return oid, true
	}
	return "", false
}

func sortedRefNames(repo *Repo) []string {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Server) writeV0RefsLocked(buf *bytes.Buffer, repo *Repo, caps string) {
	names := sortedRefNames(repo)
	first := true
	if head, ok := headOID(repo); ok {
		line := head + " HEAD"
		if first {
			line += "\x00" + caps
			first = false
		}
		writePkt(buf, line+"\n")
	}
	for _, name := range names {
		line := repo.Refs[name] + " " + name
		if first {
			line += "\x00" + caps
			first = false
		}
		writePkt(buf, line+"\n")
	}
	if first {
		line := zeroOID + " capabilities^{}\x00" + caps
		writePkt(buf, line+"\n")
	}
	writeFlush(buf)
}

// ---------------------------------------------------------------- upload-pack

func (s *Server) gitUploadPack(w http.ResponseWriter, r *http.Request, owner, name string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, errBody("Bad Request"))
		return
	}
	repo := s.gitRepo(owner, name)
	if repo == nil {
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	segments, _, perr := parsePktSegments(body)
	if perr != nil {
		writeJSON(w, 400, errBody("Bad Request"))
		return
	}
	v2 := false
	for _, line := range firstSegment(segments) {
		if line == "command=fetch" || line == "command=ls-refs" || strings.HasPrefix(line, "command=") {
			v2 = true
			break
		}
	}
	if v2 {
		s.handleUploadV2(w, repo, segments)
		return
	}
	s.handleUploadV0(w, repo, segments)
}

func firstSegment(segments [][]string) []string {
	if len(segments) == 0 {
		return nil
	}
	return segments[0]
}

func (s *Server) handleUploadV2(w http.ResponseWriter, repo *Repo, segments [][]string) {
	header := firstSegment(segments)
	args := []string{}
	if len(segments) > 1 {
		args = segments[1]
	}
	command := ""
	for _, line := range header {
		if strings.HasPrefix(line, "command=") {
			command = strings.TrimPrefix(line, "command=")
		}
	}
	var buf bytes.Buffer
	switch command {
	case "ls-refs":
		s.mu.RLock()
		s.writeLsRefsLocked(&buf, repo, args)
		s.mu.RUnlock()
	case "fetch":
		pack := s.fetchFor(repo, args)
		if len(pack.shallow) > 0 || len(pack.unshallow) > 0 {
			writePkt(&buf, "shallow-info\n")
			for _, oid := range pack.unshallow {
				writePkt(&buf, "unshallow "+oid+"\n")
			}
			for _, oid := range pack.shallow {
				writePkt(&buf, "shallow "+oid+"\n")
			}
			writeFlush(&buf)
		}
		writePkt(&buf, "packfile\n")
		writeSideband(&buf, pack.pack)
		writeFlush(&buf)
	default:
		writeFlush(&buf)
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(200)
	w.Write(buf.Bytes())
}

func writeSideband(buf *bytes.Buffer, data []byte) {
	const chunk = 65000
	if len(data) == 0 {
		return
	}
	for len(data) > 0 {
		n := len(data)
		if n > chunk {
			n = chunk
		}
		payload := make([]byte, n+1)
		payload[0] = 1
		copy(payload[1:], data[:n])
		writePktBytes(buf, payload)
		data = data[n:]
	}
}

func (s *Server) writeLsRefsLocked(buf *bytes.Buffer, repo *Repo, args []string) {
	peel := false
	symrefs := false
	unborn := false
	var prefixes []string
	for _, line := range args {
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(line, "ref-prefix "))
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if name == p || strings.HasPrefix(name, p+"/") || (strings.HasSuffix(p, "/") && strings.HasPrefix(name, p)) {
				return true
			}
		}
		return false
	}
	if len(prefixes) == 0 {
		if head, ok := headOID(repo); ok {
			line := head + " HEAD"
			if symrefs {
				line += " symref-target:refs/heads/" + repo.DefaultBranch
			}
			writePkt(buf, line+"\n")
		} else if unborn {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:refs/heads/" + repo.DefaultBranch
			}
			writePkt(buf, line+"\n")
		}
	}
	for _, name := range sortedRefNames(repo) {
		if !matches(name) {
			continue
		}
		line := repo.Refs[name] + " " + name
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if commit, ok := peelToCommit(repo, repo.Refs[name]); ok {
				line += " peeled:" + commit
			}
		}
		writePkt(buf, line+"\n")
	}
	writeFlush(buf)
}

// fetchFor computes (or returns cached) pack bytes for one fetch request.
func (s *Server) fetchFor(repo *Repo, args []string) fetchPack {
	var wants, haves, shallow []string
	depth := 0
	for _, line := range args {
		switch {
		case strings.HasPrefix(line, "want "):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				wants = append(wants, fields[1])
			}
		case strings.HasPrefix(line, "have "):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				haves = append(haves, fields[1])
			}
		case strings.HasPrefix(line, "shallow "):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				shallow = append(shallow, fields[1])
			}
		case strings.HasPrefix(line, "deepen "):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if n, err := strconv.Atoi(fields[1]); err == nil {
					depth = n
				}
			}
		}
	}
	key := "d" + strconv.Itoa(depth) + "|w" + strings.Join(wants, ",") + "|h" + strings.Join(haves, ",") + "|s" + strings.Join(shallow, ",")
	repo.packMu.Lock()
	defer repo.packMu.Unlock()
	if cached, ok := repo.packCache[key]; ok {
		return *cached
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var set map[string]bool
	var bounds []string
	if len(wants) == 0 {
		set = map[string]bool{}
	} else {
		set, bounds = wantClosure(repo, wants, depth)
	}
	haveSet := closureOf(repo, haves)
	oids := make([]string, 0, len(set))
	for oid := range set {
		if haveSet[oid] {
			continue
		}
		if _, ok := repo.Objects[oid]; ok {
			oids = append(oids, oid)
		}
	}
	sort.Strings(oids)
	objects := make([]*gObj, 0, len(oids))
	for _, oid := range oids {
		objects = append(objects, repo.Objects[oid])
	}
	pack := fetchPack{pack: buildPack(objects)}
	boundSet := make(map[string]bool, len(bounds))
	for _, oid := range bounds {
		boundSet[oid] = true
		pack.shallow = append(pack.shallow, oid)
	}
	for _, oid := range shallow {
		if !boundSet[oid] {
			pack.unshallow = append(pack.unshallow, oid)
		}
	}
	repo.packCache[key] = &pack
	return pack
}

// handleUploadV0 answers the old protocol: NAK, then a sideband pack.
func (s *Server) handleUploadV0(w http.ResponseWriter, repo *Repo, segments [][]string) {
	var wants, haves []string
	depth := 0
	for _, seg := range segments {
		for _, line := range seg {
			switch {
			case strings.HasPrefix(line, "want "):
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					wants = append(wants, fields[1])
				}
			case strings.HasPrefix(line, "have "):
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					haves = append(haves, fields[1])
				}
			case strings.HasPrefix(line, "deepen "):
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if n, err := strconv.Atoi(fields[1]); err == nil {
						depth = n
					}
				}
			}
		}
	}
	s.mu.RLock()
	var set map[string]bool
	var bounds []string
	if len(wants) > 0 {
		set, bounds = wantClosure(repo, wants, depth)
	} else {
		set = map[string]bool{}
	}
	haveSet := closureOf(repo, haves)
	objects := make([]*gObj, 0, len(set))
	for oid := range set {
		if !haveSet[oid] {
			if obj := repo.Objects[oid]; obj != nil {
				objects = append(objects, obj)
			}
		}
	}
	s.mu.RUnlock()
	var buf bytes.Buffer
	for _, oid := range bounds {
		writePkt(&buf, "shallow "+oid+"\n")
	}
	writePkt(&buf, "NAK\n")
	writeSideband(&buf, buildPack(objects))
	writeFlush(&buf)
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(200)
	w.Write(buf.Bytes())
}

// ---------------------------------------------------------------- receive-pack

func (s *Server) gitReceivePack(w http.ResponseWriter, r *http.Request, owner, name string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, errBody("Bad Request"))
		return
	}
	repo := s.gitRepo(owner, name)
	if repo == nil {
		writeJSON(w, 404, errBody("Not Found"))
		return
	}
	lines, remainder, perr := splitPktCommands(body)
	if perr != nil {
		writeJSON(w, 400, errBody("Bad Request"))
		return
	}

	type cmd struct {
		old string
		new string
		ref string
		ok  bool
		why string
	}
	var commands []cmd
	sideband := false
	for i, line := range lines {
		caps := ""
		if i == 0 {
			if idx := strings.IndexByte(line, 0); idx >= 0 {
				caps = line[idx+1:]
				line = line[:idx]
			}
			if strings.Contains(caps, "side-band-64k") {
				sideband = true
			}
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		commands = append(commands, cmd{old: fields[0], new: fields[1], ref: fields[2]})
	}

	s.mu.Lock()
	unpackOK := true
	unpackDetail := ""
	if len(remainder) > 0 {
		objects, perr := readPackDir(remainder, repo)
		if perr != nil {
			unpackOK = false
			unpackDetail = perr.Error()
		} else {
			for _, obj := range objects {
				oid := objOID(obj.typ, obj.data)
				repo.Objects[oid] = obj
			}
		}
	}
	for i := range commands {
		c := &commands[i]
		if !unpackOK {
			c.ok = false
			c.why = "unpacker error"
			continue
		}
		if !isHexOID(c.new) || !isHexOID(c.old) {
			c.ok = false
			c.why = "invalid object id"
			continue
		}
		if c.new == zeroOID {
			delete(repo.Refs, c.ref)
			c.ok = true
			continue
		}
		current, exists := repo.Refs[c.ref]
		if exists && c.old != zeroOID && c.old != current {
			c.ok = false
			c.why = "stale info"
			continue
		}
		if _, ok := repo.Objects[c.new]; !ok {
			c.ok = false
			c.why = "missing necessary objects"
			continue
		}
		if strings.HasPrefix(c.ref, "refs/heads/") && exists && !isAncestor(repo, current, c.new) {
			c.ok = false
			c.why = "non-fast-forward"
			continue
		}
		repo.Refs[c.ref] = c.new
		c.ok = true
	}
	repo.packMu.Lock()
	repo.packCache = make(map[string]*fetchPack)
	repo.packMu.Unlock()
	s.mu.Unlock()

	var inner bytes.Buffer
	if unpackOK {
		writePkt(&inner, "unpack ok\n")
	} else {
		writePkt(&inner, "unpack "+unpackDetail+"\n")
	}
	for _, c := range commands {
		if c.ok {
			writePkt(&inner, "ok "+c.ref+"\n")
		} else {
			writePkt(&inner, "ng "+c.ref+" "+c.why+"\n")
		}
	}
	writeFlush(&inner)

	var out bytes.Buffer
	if sideband {
		writeSideband(&out, inner.Bytes())
		writeFlush(&out)
	} else {
		out.Write(inner.Bytes())
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.WriteHeader(200)
	w.Write(out.Bytes())
}
