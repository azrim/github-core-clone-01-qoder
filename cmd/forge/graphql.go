package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- a small GraphQL front end

type gqlTok struct {
	kind byte // 'n' name, 'i' number, 's' string, 'p' punctuation
	s    string
}

func gqlIsNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func gqlIsNameChar(c byte) bool {
	return gqlIsNameStart(c) || (c >= '0' && c <= '9')
}

func gqlIsDigit(c byte) bool { return c >= '0' && c <= '9' }

func gqlLex(src string) ([]gqlTok, error) {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '!' || c == '$' || c == '(' || c == ')' || c == '{' || c == '}' ||
			c == '[' || c == ']' || c == ':' || c == '=' || c == '@' || c == '|' || c == '&':
			toks = append(toks, gqlTok{'p', string(c)})
			i++
		case c == '.':
			if i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.' {
				toks = append(toks, gqlTok{'p', "..."})
				i += 3
			} else {
				return nil, fmt.Errorf("unexpected '.' at offset %d", i)
			}
		case c == '"':
			i++
			var b strings.Builder
			closed := false
			for i < len(src) {
				ch := src[i]
				if ch == '"' {
					i++
					closed = true
					break
				}
				if ch != '\\' {
					b.WriteByte(ch)
					i++
					continue
				}
				i++
				if i >= len(src) {
					break
				}
				esc := src[i]
				i++
				switch esc {
				case '"':
					b.WriteByte('"')
				case '\\':
					b.WriteByte('\\')
				case '/':
					b.WriteByte('/')
				case 'b':
					b.WriteByte('\b')
				case 'f':
					b.WriteByte('\f')
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				case 'u':
					if i+4 > len(src) {
						return nil, fmt.Errorf("truncated \\u escape")
					}
					n, err := strconv.ParseUint(src[i:i+4], 16, 32)
					if err != nil {
						return nil, fmt.Errorf("bad \\u escape")
					}
					i += 4
					r := rune(n)
					if r >= 0xD800 && r <= 0xDBFF && i+6 <= len(src) && src[i] == '\\' && src[i+1] == 'u' {
						n2, err2 := strconv.ParseUint(src[i+2:i+6], 16, 32)
						if err2 == nil {
							r2 := rune(n2)
							if r2 >= 0xDC00 && r2 <= 0xDFFF {
								r = 0x10000 + (r-0xD800)<<10 + (r2 - 0xDC00)
								i += 6
							}
						}
					}
					b.WriteRune(r)
				default:
					b.WriteByte(esc)
				}
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, gqlTok{'s', b.String()})
		case c == '-' || gqlIsDigit(c):
			start := i
			if c == '-' {
				i++
			}
			for i < len(src) && gqlIsDigit(src[i]) {
				i++
			}
			if i < len(src) && src[i] == '.' {
				i++
				for i < len(src) && gqlIsDigit(src[i]) {
					i++
				}
			}
			if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
				i++
				if i < len(src) && (src[i] == '+' || src[i] == '-') {
					i++
				}
				for i < len(src) && gqlIsDigit(src[i]) {
					i++
				}
			}
			toks = append(toks, gqlTok{'i', src[start:i]})
		case gqlIsNameStart(c):
			start := i
			for i < len(src) && gqlIsNameChar(src[i]) {
				i++
			}
			toks = append(toks, gqlTok{'n', src[start:i]})
		default:
			return nil, fmt.Errorf("unexpected character %q", string(c))
		}
	}
	return toks, nil
}

type gqlVarRef struct{ name string }

type gqlField struct {
	alias string
	name  string
	args  map[string]any
	sel   []gqlSel
}

type gqlSel struct {
	field  *gqlField
	onType string
	sel    []gqlSel
}

type gqlOp struct {
	kind string
	name string
	vars map[string]any // variable name -> default value
	sel  []gqlSel
}

type gqlParser struct {
	toks []gqlTok
	pos  int
}

func (p *gqlParser) peek() (gqlTok, bool) {
	if p.pos >= len(p.toks) {
		return gqlTok{}, false
	}
	return p.toks[p.pos], true
}

func (p *gqlParser) isPunct(s string) bool {
	t, ok := p.peek()
	return ok && t.kind == 'p' && t.s == s
}

func (p *gqlParser) acceptPunct(s string) bool {
	if p.isPunct(s) {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) expectPunct(s string) error {
	if !p.acceptPunct(s) {
		return fmt.Errorf("expected %q", s)
	}
	return nil
}

func (p *gqlParser) expectName() (string, error) {
	t, ok := p.peek()
	if !ok || t.kind != 'n' {
		return "", fmt.Errorf("expected a name")
	}
	p.pos++
	return t.s, nil
}

func gqlParseDocument(src string) (*gqlOp, error) {
	toks, err := gqlLex(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	op := &gqlOp{kind: "query", vars: map[string]any{}}
	if t, ok := p.peek(); ok && t.kind == 'n' && (t.s == "query" || t.s == "mutation" || t.s == "subscription") {
		op.kind = t.s
		p.pos++
		if t2, ok2 := p.peek(); ok2 && t2.kind == 'n' {
			op.name = t2.s
			p.pos++
		}
		if p.isPunct("(") {
			p.pos++
			for !p.isPunct(")") {
				if _, ok := p.peek(); !ok {
					return nil, fmt.Errorf("unterminated variable definitions")
				}
				if err := p.expectPunct("$"); err != nil {
					return nil, err
				}
				name, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				if err := p.skipType(); err != nil {
					return nil, err
				}
				var def any
				if p.acceptPunct("=") {
					def, err = p.parseValue()
					if err != nil {
						return nil, err
					}
				}
				op.vars[name] = def
			}
			p.pos++
		}
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.sel = sel
	return op, nil
}

func (p *gqlParser) skipType() error {
	if p.acceptPunct("[") {
		if _, err := p.expectName(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else if _, err := p.expectName(); err != nil {
		return err
	}
	p.acceptPunct("!")
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]gqlSel, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var out []gqlSel
	for !p.isPunct("}") {
		if _, ok := p.peek(); !ok {
			return nil, fmt.Errorf("unterminated selection set")
		}
		if p.acceptPunct("...") {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if name == "on" {
				onType, err := p.expectName()
				if err != nil {
					return nil, err
				}
				sel, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				out = append(out, gqlSel{onType: onType, sel: sel})
			}
			continue
		}
		first, err := p.expectName()
		if err != nil {
			return nil, err
		}
		field := &gqlField{name: first, args: map[string]any{}}
		if p.acceptPunct(":") {
			field.alias = first
			field.name, err = p.expectName()
			if err != nil {
				return nil, err
			}
		}
		if p.isPunct("(") {
			p.pos++
			for !p.isPunct(")") {
				argName, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				value, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				field.args[argName] = value
			}
			p.pos++
		}
		for p.acceptPunct("@") {
			if _, err := p.expectName(); err != nil {
				return nil, err
			}
			if p.isPunct("(") {
				p.pos++
				depth := 1
				for depth > 0 {
					t, ok := p.peek()
					if !ok {
						return nil, fmt.Errorf("unterminated directive")
					}
					p.pos++
					if t.kind == 'p' && t.s == "(" {
						depth++
					} else if t.kind == 'p' && t.s == ")" {
						depth--
					}
				}
			}
		}
		if p.isPunct("{") {
			field.sel, err = p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
		}
		out = append(out, gqlSel{field: field})
	}
	p.pos++
	return out, nil
}

func (p *gqlParser) parseValue() (any, error) {
	t, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("expected a value")
	}
	switch t.kind {
	case 's':
		p.pos++
		return t.s, nil
	case 'i':
		p.pos++
		if n, err := strconv.ParseInt(t.s, 10, 64); err == nil {
			return float64(n), nil
		}
		f, err := strconv.ParseFloat(t.s, 64)
		if err != nil {
			return nil, err
		}
		return f, nil
	case 'n':
		p.pos++
		switch t.s {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		return t.s, nil
	case 'p':
		switch t.s {
		case "$":
			p.pos++
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			return gqlVarRef{name: name}, nil
		case "[":
			p.pos++
			var out []any
			for !p.isPunct("]") {
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			p.pos++
			return out, nil
		case "{":
			p.pos++
			out := map[string]any{}
			for !p.isPunct("}") {
				name, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				out[name] = v
			}
			p.pos++
			return out, nil
		}
	}
	return nil, fmt.Errorf("unexpected token while parsing a value")
}

func gqlResolveValues(v any, vars map[string]any) any {
	switch t := v.(type) {
	case gqlVarRef:
		return vars[t.name]
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = gqlResolveValues(item, vars)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = gqlResolveValues(item, vars)
		}
		return out
	}
	return v
}

// ---------------------------------------------------------------- execution values

type gqlObj struct {
	typ string
	v   any
}

type gqlList struct {
	typ   string
	items []any
}

type issueNode struct {
	repo  *Repo
	issue *Issue
}

type pullNode struct {
	repo *Repo
	pull *Pull
}

type commentNode struct{ comment *Comment }

type viewerNode struct{ login string }

type refNode struct{ name string }

type issueConn struct {
	issues  []*Issue
	hasNext bool
	end     string
}

type pullConn struct{ pulls []*Pull }

type pageInfo struct {
	hasNext bool
	end     string
}

type issuePayload struct{ node *issueNode }
type pullPayload struct{ node *pullNode }
type commentEdge struct{ node *commentNode }

const gqlPageInfoType = "PageInfo"

// ---------------------------------------------------------------- the handler

type gqlRequestBody struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

type gqlErr struct {
	Message string `json:"message"`
}

func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, errBody("Method Not Allowed"))
		return
	}
	if !s.authorized(r) {
		writeJSON(w, 401, errBody("Requires authentication"))
		return
	}
	data, err := readBody(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"errors": []gqlErr{{Message: "could not read the request body"}}})
		return
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		writeJSON(w, 400, map[string]any{"errors": []gqlErr{{Message: "the request body is empty"}}})
		return
	}
	if trimmed[0] == '[' {
		var requests []gqlRequestBody
		if err := json.Unmarshal(data, &requests); err != nil {
			writeJSON(w, 400, map[string]any{"errors": []gqlErr{{Message: "the request body is not valid JSON"}}})
			return
		}
		out := make([]map[string]any, 0, len(requests))
		for _, request := range requests {
			out = append(out, s.executeGraphQL(request))
		}
		writeJSON(w, 200, out)
		return
	}
	var request gqlRequestBody
	if err := json.Unmarshal(data, &request); err != nil {
		writeJSON(w, 400, map[string]any{"errors": []gqlErr{{Message: "the request body is not valid JSON"}}})
		return
	}
	writeJSON(w, 200, s.executeGraphQL(request))
}

func (s *Server) executeGraphQL(request gqlRequestBody) map[string]any {
	op, err := gqlParseDocument(request.Query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []gqlErr{{Message: err.Error()}}}
	}
	vars := make(map[string]any, len(request.Variables)+len(op.vars))
	for k, v := range request.Variables {
		vars[k] = v
	}
	for name, def := range op.vars {
		if _, ok := vars[name]; !ok && def != nil {
			vars[name] = gqlResolveValues(def, vars)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root := "Query"
	if op.kind == "mutation" {
		root = "Mutation"
	}
	var errs []gqlErr
	data := s.gqlRender(root, nil, op.sel, vars, &errs)
	body := map[string]any{"data": data}
	if len(errs) > 0 {
		body["errors"] = errs
	}
	return body
}

func (s *Server) gqlRender(typ string, obj any, sels []gqlSel, vars map[string]any, errs *[]gqlErr) map[string]any {
	out := make(map[string]any)
	for _, sel := range sels {
		if sel.onType != "" {
			if gqlTypeMatches(sel.onType, typ) {
				sub := s.gqlRender(typ, obj, sel.sel, vars, errs)
				for k, v := range sub {
					out[k] = v
				}
			}
			continue
		}
		field := sel.field
		key := field.name
		if field.alias != "" {
			key = field.alias
		}
		if field.name == "__typename" {
			out[key] = typ
			continue
		}
		args := make(map[string]any, len(field.args))
		for k, v := range field.args {
			args[k] = gqlResolveValues(v, vars)
		}
		value, err := s.gqlField(typ, obj, field.name, args)
		if err != nil {
			*errs = append(*errs, gqlErr{Message: err.Error()})
			out[key] = nil
			continue
		}
		out[key] = s.gqlRenderValue(value, field.sel, vars, errs)
	}
	return out
}

func (s *Server) gqlRenderValue(value any, sel []gqlSel, vars map[string]any, errs *[]gqlErr) any {
	switch t := value.(type) {
	case nil:
		return nil
	case gqlObj:
		if len(sel) == 0 {
			return t.v
		}
		return s.gqlRender(t.typ, t.v, sel, vars, errs)
	case gqlList:
		out := make([]any, 0, len(t.items))
		for _, item := range t.items {
			if item == nil {
				out = append(out, nil)
				continue
			}
			out = append(out, s.gqlRender(t.typ, item, sel, vars, errs))
		}
		return out
	default:
		return value
	}
}

func gqlTypeMatches(onType, typ string) bool {
	if onType == typ {
		return true
	}
	switch onType {
	case "Node":
		return typ == "Repository" || typ == "Issue" || typ == "PullRequest" || typ == "Comment"
	case "RepositoryOwner":
		return typ == "User"
	}
	return false
}

func gqlStrings(arg any) []string {
	list, ok := arg.([]any)
	if !ok {
		if s, ok := arg.(string); ok && s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func gqlMap(arg any) map[string]any {
	if m, ok := arg.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// gqlField resolves one field of one type. Object values come back as gqlObj, lists as gqlList.
func (s *Server) gqlField(typ string, obj any, name string, args map[string]any) (any, error) {
	switch typ {
	case "Query":
		switch name {
		case "viewer":
			return gqlObj{"User", viewerNode{s.viewer}}, nil
		case "repository":
			owner := stringFromAny(args["owner"])
			repoName := stringFromAny(args["name"])
			repo := s.repoLocked(owner, repoName)
			if repo == nil {
				return nil, nil
			}
			return gqlObj{"Repository", repo}, nil
		case "node":
			kind, id, ok := parseGlobalID(stringFromAny(args["id"]))
			if !ok {
				return nil, nil
			}
			switch kind {
			case "repository":
				repo := s.repoByIDLocked(id)
				if repo == nil {
					return nil, nil
				}
				return gqlObj{"Repository", repo}, nil
			case "issue":
				repo, is := s.findIssueByIDLocked(id)
				if is == nil {
					return nil, nil
				}
				return gqlObj{"Issue", &issueNode{repo, is}}, nil
			case "pull":
				repo, pr := s.findPullByIDLocked(id)
				if pr == nil {
					return nil, nil
				}
				return gqlObj{"PullRequest", &pullNode{repo, pr}}, nil
			}
			return nil, nil
		case "__type":
			typeName := stringFromAny(args["name"])
			if _, ok := gqlSchemaTypes[typeName]; !ok {
				return nil, nil
			}
			return gqlObj{"__Type", typeName}, nil
		case "__schema":
			return gqlObj{"__Type", "Issue"}, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "Mutation":
		input := gqlMap(args["input"])
		switch name {
		case "createIssue":
			kind, id, ok := parseGlobalID(stringFromAny(input["repositoryId"]))
			if !ok || kind != "repository" {
				return nil, fmt.Errorf("createIssue: repositoryId is not a repository id")
			}
			repo := s.repoByIDLocked(id)
			if repo == nil {
				return nil, fmt.Errorf("createIssue: repository not found")
			}
			is := s.createIssueLocked(repo, stringFromAny(input["title"]), stringFromAny(input["body"]), nil)
			return gqlObj{"CreateIssuePayload", issuePayload{&issueNode{repo, is}}}, nil
		case "closeIssue":
			kind, id, ok := parseGlobalID(stringFromAny(input["issueId"]))
			if !ok || kind != "issue" {
				return nil, fmt.Errorf("closeIssue: issueId is not an issue id")
			}
			repo, is := s.findIssueByIDLocked(id)
			if is == nil {
				return nil, fmt.Errorf("closeIssue: issue not found")
			}
			if is.State != "closed" {
				now := s.stampLocked()
				is.ClosedAt = &now
			}
			is.State = "closed"
			is.UpdatedAt = s.stampLocked()
			repo.touchIssues()
			return gqlObj{"CloseIssuePayload", issuePayload{&issueNode{repo, is}}}, nil
		case "createPullRequest":
			kind, id, ok := parseGlobalID(stringFromAny(input["repositoryId"]))
			if !ok || kind != "repository" {
				return nil, fmt.Errorf("createPullRequest: repositoryId is not a repository id")
			}
			repo := s.repoByIDLocked(id)
			if repo == nil {
				return nil, fmt.Errorf("createPullRequest: repository not found")
			}
			baseRef := splitHeadRef(stringFromAny(input["baseRefName"]))
			headRef := splitHeadRef(stringFromAny(input["headRefName"]))
			headSha, okHead := peelRef(repo, headRef)
			baseSha, okBase := peelRef(repo, baseRef)
			if !okHead || !okBase {
				return nil, fmt.Errorf("createPullRequest: the base or head branch does not exist")
			}
			pr := s.createPullLocked(repo, stringFromAny(input["title"]), stringFromAny(input["body"]), headRef, headSha, baseRef, baseSha)
			return gqlObj{"CreatePullRequestPayload", pullPayload{&pullNode{repo, pr}}}, nil
		case "mergePullRequest":
			kind, id, ok := parseGlobalID(stringFromAny(input["pullRequestId"]))
			if !ok || kind != "pull" {
				return nil, fmt.Errorf("mergePullRequest: pullRequestId is not a pull request id")
			}
			repo, pr := s.findPullByIDLocked(id)
			if pr == nil {
				return nil, fmt.Errorf("mergePullRequest: pull request not found")
			}
			out, status := s.mergePullLocked(repo, pr)
			if status != 200 {
				return nil, fmt.Errorf("%s", stringFromAny(out["message"]))
			}
			return gqlObj{"MergePullRequestPayload", pullPayload{&pullNode{repo, pr}}}, nil
		case "addComment":
			kind, id, ok := parseGlobalID(stringFromAny(input["subjectId"]))
			if !ok {
				return nil, fmt.Errorf("addComment: subjectId is not a node id")
			}
			if kind != "issue" {
				return nil, fmt.Errorf("addComment: only issue subjects are supported")
			}
			repo, is := s.findIssueByIDLocked(id)
			if is == nil {
				return nil, fmt.Errorf("addComment: issue not found")
			}
			now := s.stampLocked()
			c := &Comment{ID: s.nextCommentID, Body: stringFromAny(input["body"]), CreatedAt: now}
			s.nextCommentID++
			repo.Comments[is.Number] = append(repo.Comments[is.Number], c)
			repo.touchIssues()
			return gqlObj{"AddCommentPayload", commentEdge{&commentNode{c}}}, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "User":
		switch name {
		case "login":
			login := ""
			switch t := obj.(type) {
			case viewerNode:
				login = t.login
			case string:
				login = t
			}
			if login == "" {
				login = s.viewer
			}
			return login, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "Repository":
		repo, ok := obj.(*Repo)
		if !ok {
			return nil, nil
		}
		switch name {
		case "id":
			return globalID("repository", repo.ID), nil
		case "databaseId":
			return repo.ID, nil
		case "name":
			return repo.Name, nil
		case "nameWithOwner":
			return s.viewer + "/" + repo.Name, nil
		case "owner":
			return gqlObj{"User", viewerNode{s.viewer}}, nil
		case "defaultBranchRef":
			return gqlObj{"Ref", refNode{repo.DefaultBranch}}, nil
		case "viewerPermission":
			return "ADMIN", nil
		case "hasIssuesEnabled":
			return true, nil
		case "hasPullRequestsEnabled":
			return true, nil
		case "isPrivate":
			return repo.Private, nil
		case "description":
			return repo.Description, nil
		case "isArchived":
			return false, nil
		case "stargazerCount":
			return 0, nil
		case "createdAt":
			return "2026-01-01T00:00:00Z", nil
		case "updatedAt":
			return "2026-01-01T00:00:00Z", nil
		case "issue":
			is := repo.issueByNumberLocked(intFromAny(args["number"], 0))
			if is == nil {
				return nil, nil
			}
			return gqlObj{"Issue", &issueNode{repo, is}}, nil
		case "issues":
			return gqlObj{"IssueConnection", s.gqlIssuesConnection(repo, args)}, nil
		case "pullRequest":
			pr := repo.pullByNumberLocked(intFromAny(args["number"], 0))
			if pr == nil {
				return nil, nil
			}
			return gqlObj{"PullRequest", &pullNode{repo, pr}}, nil
		case "pullRequests":
			first := intFromAny(args["first"], 30)
			if first <= 0 {
				first = 30
			}
			pulls := make([]*Pull, 0, len(repo.Pulls))
			for _, pr := range repo.Pulls {
				if pr.State == "open" {
					pulls = append(pulls, pr)
				}
			}
			sort.SliceStable(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
			if len(pulls) > first {
				pulls = pulls[:first]
			}
			return gqlObj{"PullRequestConnection", &pullConn{pulls}}, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "Issue":
		node, ok := obj.(*issueNode)
		if !ok {
			return nil, nil
		}
		is := node.issue
		switch name {
		case "id":
			return globalID("issue", is.ID), nil
		case "number":
			return is.Number, nil
		case "title":
			return is.Title, nil
		case "body":
			return is.Body, nil
		case "state":
			return strings.ToUpper(is.State), nil
		case "createdAt":
			return is.CreatedAt, nil
		case "updatedAt":
			return is.UpdatedAt, nil
		case "closed":
			return is.State == "closed", nil
		case "author":
			return gqlObj{"User", viewerNode{s.viewer}}, nil
		case "repository":
			return gqlObj{"Repository", node.repo}, nil
		case "comments":
			comments := node.repo.Comments[is.Number]
			items := make([]any, 0, len(comments))
			for _, c := range comments {
				items = append(items, &commentNode{c})
			}
			return gqlList{"Comment", items}, nil
		case "url":
			return "http://forge.local/" + s.viewer + "/" + node.repo.Name + "/issues/" + strconv.Itoa(is.Number), nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "PullRequest":
		node, ok := obj.(*pullNode)
		if !ok {
			return nil, nil
		}
		pr := node.pull
		switch name {
		case "id":
			return globalID("pull", pr.ID), nil
		case "number":
			return pr.Number, nil
		case "title":
			return pr.Title, nil
		case "body":
			return pr.Body, nil
		case "state":
			if pr.Merged {
				return "MERGED", nil
			}
			return strings.ToUpper(pr.State), nil
		case "merged":
			return pr.Merged, nil
		case "mergeable":
			return "MERGEABLE", nil
		case "headRefName":
			return pr.HeadRef, nil
		case "baseRefName":
			return pr.BaseRef, nil
		case "headRefOid":
			return pr.HeadSha, nil
		case "baseRefOid":
			return pr.BaseSha, nil
		case "createdAt":
			return pr.CreatedAt, nil
		case "updatedAt":
			return pr.UpdatedAt, nil
		case "author":
			return gqlObj{"User", viewerNode{s.viewer}}, nil
		case "repository":
			return gqlObj{"Repository", node.repo}, nil
		case "files":
			first := intFromAny(args["first"], 10)
			if first <= 0 {
				first = 10
			}
			paths := s.pullChangedPaths(node.repo, pr)
			if len(paths) > first {
				paths = paths[:first]
			}
			return gqlObj{"PullRequestChangedFileConnection", paths}, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "PullRequestChangedFileConnection":
		paths, _ := obj.([]string)
		switch name {
		case "nodes":
			items := make([]any, 0, len(paths))
			for _, path := range paths {
				items = append(items, path)
			}
			return gqlList{"PullRequestChangedFile", items}, nil
		case "totalCount":
			return len(paths), nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "PullRequestChangedFile":
		path, _ := obj.(string)
		switch name {
		case "path":
			return path, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "Comment":
		node, ok := obj.(*commentNode)
		if !ok {
			return nil, nil
		}
		switch name {
		case "id":
			return globalID("comment", node.comment.ID), nil
		case "body":
			return node.comment.Body, nil
		case "createdAt":
			return node.comment.CreatedAt, nil
		case "author":
			return gqlObj{"User", viewerNode{s.viewer}}, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "Ref":
		ref, _ := obj.(refNode)
		switch name {
		case "name":
			return ref.name, nil
		case "target":
			return nil, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "IssueConnection":
		conn, ok := obj.(*issueConn)
		if !ok {
			return nil, nil
		}
		switch name {
		case "nodes":
			items := make([]any, 0, len(conn.issues))
			for _, is := range conn.issues {
				items = append(items, &issueNode{s.repoByIssueLocked(is), is})
			}
			return gqlList{"Issue", items}, nil
		case "pageInfo":
			return gqlObj{gqlPageInfoType, pageInfo{conn.hasNext, conn.end}}, nil
		case "totalCount":
			return len(conn.issues), nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "PullRequestConnection":
		conn, ok := obj.(*pullConn)
		if !ok {
			return nil, nil
		}
		switch name {
		case "nodes":
			items := make([]any, 0, len(conn.pulls))
			for _, pr := range conn.pulls {
				items = append(items, &pullNode{s.repoByPullLocked(pr), pr})
			}
			return gqlList{"PullRequest", items}, nil
		case "totalCount":
			return len(conn.pulls), nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "PageInfo":
		info, ok := obj.(pageInfo)
		if !ok {
			return nil, nil
		}
		switch name {
		case "hasNextPage":
			return info.hasNext, nil
		case "hasPreviousPage":
			return false, nil
		case "endCursor":
			if info.end == "" {
				return nil, nil
			}
			return info.end, nil
		case "startCursor":
			if info.end == "" {
				return nil, nil
			}
			return info.end, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "__Type":
		typeName, _ := obj.(string)
		spec, ok := gqlSchemaTypes[typeName]
		if !ok {
			return nil, nil
		}
		switch name {
		case "name":
			return typeName, nil
		case "kind":
			return spec.kind, nil
		case "fields":
			items := make([]any, 0, len(spec.fields))
			for _, fieldName := range spec.fields {
				items = append(items, fieldName)
			}
			return gqlList{"__Field", items}, nil
		case "enumValues":
			items := make([]any, 0, len(spec.enumValues))
			for _, enumName := range spec.enumValues {
				items = append(items, enumName)
			}
			return gqlList{"__EnumValue", items}, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "__Field":
		fieldName, _ := obj.(string)
		switch name {
		case "name":
			return fieldName, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "__EnumValue":
		enumName, _ := obj.(string)
		switch name {
		case "name":
			return enumName, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "CreateIssuePayload", "CloseIssuePayload":
		payload, ok := obj.(issuePayload)
		if !ok {
			return nil, nil
		}
		switch name {
		case "issue":
			return gqlObj{"Issue", payload.node}, nil
		case "clientMutationId":
			return nil, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "CreatePullRequestPayload", "MergePullRequestPayload":
		payload, ok := obj.(pullPayload)
		if !ok {
			return nil, nil
		}
		switch name {
		case "pullRequest":
			return gqlObj{"PullRequest", payload.node}, nil
		case "clientMutationId":
			return nil, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "AddCommentPayload":
		payload, ok := obj.(commentEdge)
		if !ok {
			return nil, nil
		}
		switch name {
		case "commentEdge":
			return gqlObj{"CommentEdge", payload}, nil
		case "clientMutationId":
			return nil, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)

	case "CommentEdge":
		edge, ok := obj.(commentEdge)
		if !ok {
			return nil, nil
		}
		switch name {
		case "node":
			return gqlObj{"Comment", edge.node}, nil
		case "cursor":
			return nil, nil
		}
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)
	}
	return nil, fmt.Errorf("Cannot query field %q on type %q.", name, typ)
}

// gqlIssuesConnection applies states, orderBy, first and the after cursor.
func (s *Server) gqlIssuesConnection(repo *Repo, args map[string]any) *issueConn {
	states := gqlStrings(args["states"])
	stateSet := make(map[string]bool, len(states))
	for _, state := range states {
		stateSet[strings.ToLower(state)] = true
	}
	list := make([]*Issue, 0, len(repo.Issues))
	for _, is := range repo.Issues {
		if len(stateSet) > 0 && !stateSet[is.State] {
			continue
		}
		list = append(list, is)
	}
	orderBy := gqlMap(args["orderBy"])
	direction := stringFromAny(orderBy["direction"])
	if direction == "" {
		direction = "ASC"
	}
	field := stringFromAny(orderBy["field"])
	less := func(i, j int) bool {
		switch field {
		case "CREATED_AT", "UPDATED_AT":
			return list[i].Number < list[j].Number
		case "COMMENTS":
			if len(repo.Comments[list[i].Number]) != len(repo.Comments[list[j].Number]) {
				return len(repo.Comments[list[i].Number]) < len(repo.Comments[list[j].Number])
			}
			return list[i].Number < list[j].Number
		default:
			return list[i].Number < list[j].Number
		}
	}
	sort.SliceStable(list, less)
	if direction == "DESC" {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	if after := stringFromAny(args["after"]); after != "" {
		if n, err := strconv.Atoi(after); err == nil {
			kept := list[:0]
			if direction == "DESC" {
				for _, is := range list {
					if is.Number < n {
						kept = append(kept, is)
					}
				}
			} else {
				for _, is := range list {
					if is.Number > n {
						kept = append(kept, is)
					}
				}
			}
			list = kept
		}
	}
	first := intFromAny(args["first"], 30)
	if first <= 0 {
		first = 30
	}
	hasNext := len(list) > first
	if hasNext {
		list = list[:first]
	}
	end := ""
	if len(list) > 0 {
		end = strconv.Itoa(list[len(list)-1].Number)
	}
	return &issueConn{issues: list, hasNext: hasNext, end: end}
}

func (s *Server) pullChangedPaths(repo *Repo, pr *Pull) []string {
	headTree := treeOfCommit(repo, pr.HeadSha)
	baseTip, ok := peelRef(repo, pr.BaseRef)
	if !ok || baseTip == "" {
		baseTip = pr.BaseSha
	}
	baseTree := treeOfCommit(repo, baseTip)
	compareTree := baseTree
	if mb := mergeBaseOf(repo, baseTip, pr.HeadSha); mb != "" {
		if t := treeOfCommit(repo, mb); t != "" {
			compareTree = t
		}
	}
	return diffTreePaths(repo, compareTree, headTree)
}

// repoByIssueLocked and repoByPullLocked recover the owning repository for list nodes. The
// connection is always built from a repository, so they are only a fallback.
func (s *Server) repoByIssueLocked(is *Issue) *Repo {
	for _, repo := range s.repos {
		for _, candidate := range repo.Issues {
			if candidate == is {
				return repo
			}
		}
	}
	return nil
}

func (s *Server) repoByPullLocked(pr *Pull) *Repo {
	for _, repo := range s.repos {
		for _, candidate := range repo.Pulls {
			if candidate == pr {
				return repo
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- the schema surface gh probes

type gqlTypeSpec struct {
	kind       string
	fields     []string
	enumValues []string
}

var gqlSchemaTypes = map[string]gqlTypeSpec{
	"Issue": {
		kind:   "OBJECT",
		fields: []string{"id", "number", "title", "state", "body", "author", "createdAt", "updatedAt", "closed", "closedAt", "labels", "comments", "repository", "url"},
	},
	"PullRequest": {
		kind:   "OBJECT",
		fields: []string{"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "files", "author", "createdAt", "updatedAt", "repository"},
	},
	"Repository": {
		kind:   "OBJECT",
		fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "hasPullRequestsEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "isPrivate", "isArchived", "stargazerCount", "createdAt", "updatedAt"},
	},
	"SearchType": {
		kind:       "ENUM",
		enumValues: []string{"ISSUE", "ISSUE_ADVANCED", "REPOSITORY", "USER", "DISCUSSION", "CODE", "COMMIT"},
	},
}
