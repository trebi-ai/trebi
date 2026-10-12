// Package ghfake is a fake GitHub API for the connector link tests and the
// link eval. It serves commits, tarballs (through a redirect to a second
// codeload server), and raw contents from in-memory trees.
package ghfake

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"
)

// File is one file of a tree. Link makes it a symlink to the target.
type File struct {
	Body string
	Exec bool
	Link string
}

// Server is the fake API and its codeload host.
type Server struct {
	API      *httptest.Server
	Codeload *httptest.Server

	mu       sync.Mutex
	refs     map[string]string          // repo@ref → sha
	trees    map[string]map[string]File // sha → path → file
	repoOf   map[string]string          // sha → repo
	token    string                     // "": no repo is private
	private  map[string]bool
	limited  bool
	level    int
	requests []Request
}

// Request is one request that a server saw.
type Request struct {
	Host string
	Path string
	Auth string
}

// New starts the two servers. Close stops them.
func New() *Server {
	s := &Server{refs: map[string]string{}, trees: map[string]map[string]File{}, repoOf: map[string]string{},
		private: map[string]bool{}, level: gzip.DefaultCompression}
	s.Codeload = httptest.NewServer(http.HandlerFunc(s.codeload))
	s.API = httptest.NewServer(http.HandlerFunc(s.api))
	return s
}

// Close stops both servers.
func (s *Server) Close() {
	s.API.Close()
	s.Codeload.Close()
}

// Push stores a tree as the new head of repo@ref and returns its sha.
func (s *Server) Push(repo, ref string, files map[string]File) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := sha256.New()
	fmt.Fprintf(h, "%s@%s %d", repo, ref, len(s.trees))
	for _, k := range slices.Sorted(maps.Keys(files)) {
		fmt.Fprintf(h, "%s=%+v;", k, files[k])
	}
	sha := hex.EncodeToString(h.Sum(nil))[:40]
	s.trees[sha] = maps.Clone(files)
	s.repoOf[sha] = repo
	s.refs[repo+"@"+ref] = sha
	if _, ok := s.refs[repo+"@HEAD"]; !ok || ref == "main" {
		s.refs[repo+"@HEAD"] = sha
	}
	return sha
}

// Rewrite replaces the tree of a commit, as a host that changes a commit
// would.
func (s *Server) Rewrite(sha string, files map[string]File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trees[sha] = maps.Clone(files)
}

// Private makes repo answer 404 unless the request has the token.
func (s *Server) Private(repo, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.private[repo], s.token = true, token
}

// RateLimit makes every API call answer 403 with no remaining calls.
func (s *Server) RateLimit(on bool) {
	s.mu.Lock()
	s.limited = on
	s.mu.Unlock()
}

// Compression sets the gzip level of the tarballs.
func (s *Server) Compression(level int) {
	s.mu.Lock()
	s.level = level
	s.mu.Unlock()
}

// Requests returns the requests so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

func (s *Server) note(r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, Request{Host: r.Host, Path: r.URL.Path, Auth: r.Header.Get("Authorization")})
	s.mu.Unlock()
}

// allowed reports whether the request may read repo.
func (s *Server) allowed(r *http.Request, repo string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.private[repo] || r.Header.Get("Authorization") == "Bearer "+s.token
}

// badToken reports a token that is not the one of Private, as GitHub
// answers 401 to bad credentials.
func (s *Server) badToken(r *http.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	auth := r.Header.Get("Authorization")
	return s.token != "" && auth != "" && auth != "Bearer "+s.token
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	s.note(r)
	s.mu.Lock()
	limited := s.limited
	s.mu.Unlock()
	if limited {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Reset", fmt.Sprint(time.Now().Add(90*time.Second).Unix()))
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		return
	}
	if s.badToken(r) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	segs := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/repos/"), "/", 4)
	if len(segs) < 4 {
		http.NotFound(w, r)
		return
	}
	repo, kind, rest := segs[0]+"/"+segs[1], segs[2], segs[3]
	if !s.allowed(r, repo) {
		http.NotFound(w, r)
		return
	}
	switch kind {
	case "commits":
		s.mu.Lock()
		sha, ok := s.refs[repo+"@"+rest]
		if !ok && s.repoOf[rest] == repo {
			sha, ok = rest, true
		}
		s.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"No commit found"}`, http.StatusUnprocessableEntity)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": sha}) //nolint:errchkjson // test server
	case "tarball":
		http.Redirect(w, r, s.Codeload.URL+"/"+repo+"/legacy.tar.gz/"+rest, http.StatusFound)
	case "contents":
		s.mu.Lock()
		f, ok := s.trees[r.URL.Query().Get("ref")][rest]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(f.Body))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) codeload(w http.ResponseWriter, r *http.Request) {
	s.note(r)
	segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(segs) != 4 {
		http.NotFound(w, r)
		return
	}
	repo, sha := segs[0]+"/"+segs[1], segs[3]
	s.mu.Lock()
	tree, ok := s.trees[sha]
	level := s.level
	s.mu.Unlock()
	if !ok || !s.allowed(r, repo) {
		http.NotFound(w, r)
		return
	}
	top := strings.ReplaceAll(repo, "/", "-") + "-" + sha[:7]
	data, err := Tarball(top, tree, level)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

// Tarball builds a tar.gz with each file under top/, as GitHub does.
func Tarball(top string, files map[string]File, level int) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "fake"}}); err != nil {
		return nil, err
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top + "/", Mode: 0o755}); err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f := files[name]
		hdr := &tar.Header{Name: top + "/" + name, Mode: 0o644, Size: int64(len(f.Body)), Typeflag: tar.TypeReg}
		if strings.HasPrefix(name, "/") {
			hdr.Name = name
		}
		if f.Exec {
			hdr.Mode = 0o755
		}
		if f.Link != "" {
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeSymlink, f.Link, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if f.Link == "" {
			if _, err := tw.Write([]byte(f.Body)); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
