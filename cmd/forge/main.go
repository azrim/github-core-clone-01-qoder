// Command forge is a single-process GitHub-compatible forge: REST v3, GraphQL v4, MCP and
// smart-HTTP git, all backed by one in-memory store. It registers its ephemeral port with the
// trial env and waits there until the env says the trial is over.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// requestID is stamped on every REST reply; the suite only requires a non-empty value.
const requestID = "forge-a71c3d5e"

// Server carries both the store and the HTTP surface.
type Server struct {
	mu      sync.RWMutex
	token   string
	viewer  string
	adopted bool

	repos         map[string]*Repo
	nextRepoID    int
	nextIssueID   int
	nextCommentID int
	nextPullID    int
	clock         int64
}

func newServer() *Server {
	return &Server{
		token:         os.Getenv("MAKEFUN_SHOP_TOKEN"),
		repos:         make(map[string]*Repo),
		nextRepoID:    4200,
		nextIssueID:   600000,
		nextCommentID: 800000,
		nextPullID:    700000,
	}
}

func main() {
	s := newServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
		}
	}()

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[forge] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := register(envURL, port); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] register: %v\n", err)
		os.Exit(1)
	}
	waitDone(envURL)
	srv.Close()
	os.Exit(0)
}

var envClient = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}

func retryDelay(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	d := 250 * time.Millisecond * time.Duration(1<<uint(attempt))
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

func register(envURL string, port int) error {
	body := fmt.Sprintf(`{"port":%d}`, port)
	for attempt := 0; ; attempt++ {
		resp, err := envClient.Post(envURL+"/register", "application/json", strings.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", resp.StatusCode)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitDone(envURL string) {
	for attempt := 0; ; attempt++ {
		resp, err := envClient.Get(envURL + "/done")
		if err == nil {
			data, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var body struct {
					Done bool `json:"done"`
				}
				if json.Unmarshal(data, &body) == nil && body.Done {
					return
				}
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-GitHub-Request-Id", requestID)
	path := r.URL.Path
	switch {
	case path == "/healthz":
		writeJSON(w, 200, map[string]any{"ok": true})
	case strings.HasPrefix(path, "/api/graphql"):
		s.handleGraphQL(w, r)
	case path == "/mcp" || path == "/mcp/":
		s.handleMCP(w, r)
	case strings.HasPrefix(path, "/api/v3"):
		if !s.authorized(r) {
			writeJSON(w, 401, errBody("Requires authentication"))
			return
		}
		s.handleREST(w, r, strings.TrimPrefix(path, "/api/v3"))
	default:
		if owner, repo, suffix, ok := parseGitPath(path); ok {
			s.handleGit(w, r, owner, repo, suffix)
			return
		}
		writeJSON(w, 404, errBody("Not Found"))
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return r.Header.Get("Authorization") != ""
	}
	return r.Header.Get("Authorization") == "Bearer "+s.token
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"message":"internal error"}`)
		status = 500
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(b)
}

func errBody(msg string) map[string]any {
	return map[string]any{"message": msg}
}

func decodeBody(r *http.Request, v any) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}
