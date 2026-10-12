// Package mockgraph is an in-memory fake of the parts of Microsoft Graph (and
// the OAuth2 token endpoint) that onedriver talks to, served by
// net/http/httptest. It lets the test suites run offline, without a OneDrive
// account.
//
// It deliberately does not import package graph, so that package graph's own
// tests can use it. Tests build their *graph.Auth from Server.TokenURL(),
// AccessToken and RefreshToken.
//
// The Server also exposes helpers (PutFile, Modify, Delete, ...) that mutate
// the "server-side" state directly, to simulate changes made from another
// device.
package mockgraph

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jstaf/onedriver/fs/graph/quickxorhash"
)

const (
	// AccessToken is the only bearer token the mock accepts.
	AccessToken = "mock-access-token"
	// RefreshToken is handed out by the token endpoint.
	RefreshToken = "mock-refresh-token"
	// Account is the userPrincipalName of the fake user.
	Account = "mock.user@example.com"

	// RootID is the ID of the drive root. It is intentionally not "root" so
	// that code using the "root" alias is exercised the way it is against the
	// real service.
	RootID = "MOCKROOT000000001"

	driveID  = "mockdrive0001"
	pageSize = 200
)

type node struct {
	id, name, parent string
	dir              bool
	data             []byte
	created, mtime   time.Time
	ver, cver        int   // eTag / cTag counters
	seq              int64 // sequence number of the last change, for delta
	deleted          bool
	facet            string // "vault" or "package:<type>": neither file nor folder
}

// Server is a fake Microsoft Graph server.
type Server struct {
	// HTTP is the underlying test server.
	HTTP *httptest.Server

	mu      sync.Mutex
	nodes   map[string]*node
	seq     int64
	counter int
	uploads map[string]*uploadSession

	pauseMu sync.Mutex
	pauseCV *sync.Cond
	paused  map[string]bool // path prefixes whose uploads are held
	failing map[string]int  // path prefixes whose uploads are rejected, by status
	failDL  map[string]int  // path prefixes whose downloads are rejected, by status

	reqMu      sync.Mutex
	requests   []string
	tokenMode  TokenMode
	tokenForms []url.Values
}

// TokenMode selects how the token endpoint answers refresh_token grants.
type TokenMode int

const (
	// TokenOK issues tokens (the default).
	TokenOK TokenMode = iota
	// TokenInvalidGrant rejects the refresh token with 400 invalid_grant.
	TokenInvalidGrant
	// TokenUnavailable answers 503, like a server having a bad day.
	TokenUnavailable
)

// SetTokenMode changes how refresh_token grants are answered. Exchanging an
// authorization code (login) always works.
func (s *Server) SetTokenMode(m TokenMode) {
	s.reqMu.Lock()
	s.tokenMode = m
	s.reqMu.Unlock()
}

// TokenForms returns the (decoded) form bodies of all token requests so far.
func (s *Server) TokenForms() []url.Values {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	return append([]url.Values(nil), s.tokenForms...)
}

// New starts a mock server containing an empty drive (with a "Documents"
// folder, like a fresh account). Call Close when done.
func New() *Server {
	s := &Server{
		nodes:   make(map[string]*node),
		uploads: make(map[string]*uploadSession),
		paused:  make(map[string]bool),
		failing: make(map[string]int),
		failDL:  make(map[string]int),
	}
	s.pauseCV = sync.NewCond(&s.pauseMu)
	now := time.Now().UTC()
	s.nodes[RootID] = &node{id: RootID, name: "root", dir: true, created: now, mtime: now, ver: 1, cver: 1}
	s.HTTP = httptest.NewServer(http.HandlerFunc(s.serve))
	s.Mkdir("/Documents")
	return s
}

// Close shuts the server down.
func (s *Server) Close() {
	s.pauseMu.Lock()
	s.paused = make(map[string]bool)
	s.pauseCV.Broadcast()
	s.pauseMu.Unlock()
	s.HTTP.Close()
}

// GraphURL is the value to assign to graph.GraphURL.
func (s *Server) GraphURL() string { return s.HTTP.URL + "/v1.0" }

// TokenURL is the value to use for graph.AuthConfig.TokenURL.
func (s *Server) TokenURL() string { return s.HTTP.URL + "/oauth2/v2.0/token" }

// Requests returns a log of "METHOD /path" for all requests seen so far.
func (s *Server) Requests() []string {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	return append([]string(nil), s.requests...)
}

// PauseUploads makes all requests that write file content (simple PUTs,
// createUploadSession and upload chunks) for items at or below pathPrefix
// (e.g. "/onedriver_tests/conflict") block until ResumeUploads is called with
// the same prefix. Uploads elsewhere are unaffected, so parallel tests do not
// interfere with each other.
func (s *Server) PauseUploads(pathPrefix string) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	s.paused[path.Clean("/"+pathPrefix)] = true
}

// ResumeUploads releases requests blocked by PauseUploads.
func (s *Server) ResumeUploads(pathPrefix string) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	delete(s.paused, path.Clean("/"+pathPrefix))
	s.pauseCV.Broadcast()
}

func under(p, prefix string) bool {
	return prefix == "/" || p == prefix || strings.HasPrefix(p, prefix+"/")
}

func (s *Server) isPaused(p string) bool {
	for prefix := range s.paused {
		if under(p, prefix) {
			return true
		}
	}
	return false
}

// FailUploads makes all requests that write file content for items at or below
// pathPrefix fail with the given HTTP status (e.g. 400 or 503) until
// StopFailingUploads is called with the same prefix.
func (s *Server) FailUploads(pathPrefix string, status int) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	s.failing[path.Clean("/"+pathPrefix)] = status
}

// StopFailingUploads undoes FailUploads.
func (s *Server) StopFailingUploads(pathPrefix string) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	delete(s.failing, path.Clean("/"+pathPrefix))
}

// failStatus returns the status uploads to p must fail with, or 0.
func (s *Server) failStatus(p string) int {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	for prefix, status := range s.failing {
		if under(p, prefix) {
			return status
		}
	}
	return 0
}

// FailDownloads makes content downloads for items at or below pathPrefix fail
// with the given HTTP status until StopFailingDownloads is called with the
// same prefix.
func (s *Server) FailDownloads(pathPrefix string, status int) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	s.failDL[path.Clean("/"+pathPrefix)] = status
}

// StopFailingDownloads undoes FailDownloads.
func (s *Server) StopFailingDownloads(pathPrefix string) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	delete(s.failDL, path.Clean("/"+pathPrefix))
}

// failDownloadStatus returns the status downloads of p must fail with, or 0.
func (s *Server) failDownloadStatus(p string) int {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	for prefix, status := range s.failDL {
		if under(p, prefix) {
			return status
		}
	}
	return 0
}

// waitPaused blocks while uploads to path p are paused.
func (s *Server) waitPaused(p string) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	for s.isPaused(p) {
		s.pauseCV.Wait()
	}
}

// ---- helpers for tests: mutate "server-side" state directly ----

// Mkdir creates a folder (and any missing parents), returning its ID. It
// returns the existing ID if the folder already exists.
func (s *Server) Mkdir(p string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mkdirAll(p).id
}

// PutVault creates an item shaped like the locked Personal Vault in the drive
// root: it has a specialFolder facet but neither a file nor a folder facet,
// its content endpoint returns 404 and listing its children fails. It returns
// the item's ID.
func (s *Server) PutVault(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.create(RootID, name, false)
	n.facet = "vault"
	return n.id
}

// PutPackage creates an item with a package facet of the given type (for
// example "oneNote") and neither a file nor a folder facet, in directory dir
// (created if missing). It returns the item's ID.
func (s *Server) PutPackage(dir, name, typ string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.create(s.mkdirAll(dir).id, name, false)
	n.facet = "package:" + typ
	return n.id
}

// PutFile creates or replaces the file at p (creating missing parent
// folders), returning its ID.
func (s *Server) PutFile(p string, content []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, name := path.Split(path.Clean("/" + p))
	parent := s.mkdirAll(dir)
	return s.putContent(parent.id, name, content, time.Time{}).id
}

// Modify replaces the content of the file with the given ID, as if another
// device had edited it.
func (s *Server) Modify(id string, content []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.live(id)
	if n == nil || n.dir {
		panic("mockgraph: Modify: no such file: " + id)
	}
	s.setContent(n, content, time.Time{})
}

// Delete removes an item (and, for folders, everything below it).
func (s *Server) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.live(id); n != nil {
		s.remove(n)
	}
}

// Move renames and/or moves an item. An empty newName or newParentID keeps
// the current one. It replaces any existing item with the same name at the
// destination.
func (s *Server) Move(id, newName, newParentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.live(id)
	if n == nil {
		return fmt.Errorf("no such item %s", id)
	}
	_, err := s.move(n, newName, newParentID, true)
	return err
}

// ID returns the ID of the item at path p, or "" if it does not exist.
func (s *Server) ID(p string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.resolve(p); n != nil {
		return n.id
	}
	return ""
}

// Exists reports whether an item exists at path p.
func (s *Server) Exists(p string) bool { return s.ID(p) != "" }

// Content returns a copy of the content of the file with the given ID.
func (s *Server) Content(id string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.live(id)
	if n == nil || n.dir {
		return nil, false
	}
	return append([]byte(nil), n.data...), true
}

// ContentPath returns a copy of the content of the file at p.
func (s *Server) ContentPath(p string) ([]byte, bool) {
	id := s.ID(p)
	if id == "" {
		return nil, false
	}
	return s.Content(id)
}

// List returns the sorted names of the live children of the folder at p.
func (s *Server) List(p string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.resolve(p)
	if dir == nil {
		return nil
	}
	var names []string
	for _, c := range s.children(dir.id) {
		names = append(names, c.name)
	}
	return names
}

// ---- internals; the mutex must be held ----

func (s *Server) live(id string) *node {
	if id == "root" {
		id = RootID
	}
	n := s.nodes[id]
	if n == nil || n.deleted {
		return nil
	}
	return n
}

func (s *Server) newID() string {
	s.counter++
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("MOCK%06X%X", s.counter, b)
}

func (s *Server) bump(n *node, content bool) {
	s.seq++
	n.seq = s.seq
	n.ver++
	if content {
		n.cver++
	}
	n.mtime = time.Now().UTC()
}

// touchParent updates a folder's eTag/mtime the way OneDrive does whenever one
// of its children changes, so that the folder shows up in delta.
func (s *Server) touchParent(id string) {
	if p := s.live(id); p != nil {
		s.bump(p, false)
	}
}

func (s *Server) children(id string) []*node {
	var out []*node
	for _, n := range s.nodes {
		if n.parent == id && !n.deleted {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].name), strings.ToLower(out[j].name)
		if a != b {
			return a < b
		}
		return out[i].id < out[j].id
	})
	return out
}

func (s *Server) child(parentID, name string) *node {
	for _, n := range s.nodes {
		if n.parent == parentID && !n.deleted && strings.EqualFold(n.name, name) {
			return n
		}
	}
	return nil
}

// resolve finds an item by "/a/b/c" style path.
func (s *Server) resolve(p string) *node {
	cur := s.live(RootID)
	for _, part := range strings.Split(path.Clean("/"+p), "/") {
		if part == "" {
			continue
		}
		if cur = s.child(cur.id, part); cur == nil {
			return nil
		}
	}
	return cur
}

func (s *Server) mkdirAll(p string) *node {
	cur := s.live(RootID)
	for _, part := range strings.Split(path.Clean("/"+p), "/") {
		if part == "" {
			continue
		}
		next := s.child(cur.id, part)
		if next == nil {
			next = s.create(cur.id, part, true)
		}
		cur = next
	}
	return cur
}

func (s *Server) create(parentID, name string, dir bool) *node {
	now := time.Now().UTC()
	n := &node{id: s.newID(), name: name, parent: parentID, dir: dir, created: now, mtime: now, ver: 1, cver: 1}
	s.nodes[n.id] = n
	s.bump(n, true)
	s.touchParent(parentID)
	return n
}

// putContent creates or replaces a file's content.
func (s *Server) putContent(parentID, name string, content []byte, mtime time.Time) *node {
	n := s.child(parentID, name)
	if n == nil {
		n = s.create(parentID, name, false)
	}
	s.setContent(n, content, mtime)
	return n
}

func (s *Server) setContent(n *node, content []byte, mtime time.Time) {
	n.data = append([]byte(nil), content...)
	s.bump(n, true)
	if !mtime.IsZero() {
		n.mtime = mtime.UTC()
	}
	s.touchParent(n.parent)
}

func (s *Server) remove(n *node) {
	for _, c := range s.children(n.id) {
		s.remove(c)
	}
	n.deleted = true
	s.bump(n, false)
	s.touchParent(n.parent)
}

func (s *Server) isDescendant(id, ancestor string) bool {
	for cur := s.nodes[id]; cur != nil; cur = s.nodes[cur.parent] {
		if cur.id == ancestor {
			return true
		}
	}
	return false
}

// move renames/moves n. If replace is false an existing item at the
// destination yields errNameExists.
func (s *Server) move(n *node, newName, newParentID string, replace bool) (*node, error) {
	if newName == "" {
		newName = n.name
	}
	if newParentID == "" {
		newParentID = n.parent
	}
	dest := s.live(newParentID)
	if dest == nil || !dest.dir {
		return nil, errNotFound
	}
	if s.isDescendant(dest.id, n.id) {
		return nil, errBadRequest
	}
	if other := s.child(dest.id, newName); other != nil && other != n {
		if !replace {
			return nil, errNameExists
		}
		s.remove(other)
	}
	oldParent := n.parent
	n.name, n.parent = newName, dest.id
	s.bump(n, false)
	s.touchParent(oldParent)
	s.touchParent(dest.id)
	return n, nil
}

// pathOf returns the "/a/b/name" path of a node.
func (s *Server) pathOf(n *node) string {
	if n.id == RootID {
		return "/"
	}
	return path.Join(strings.TrimPrefix(s.parentPath(n), "/drive/root:")+"/", n.name)
}

// parentPath returns the "/drive/root:/a/b" style path of a node's parent.
func (s *Server) parentPath(n *node) string {
	var parts []string
	for cur := s.nodes[n.parent]; cur != nil && cur.id != RootID; cur = s.nodes[cur.parent] {
		parts = append([]string{cur.name}, parts...)
	}
	return "/drive/root:" + strings.TrimSuffix("/"+strings.Join(parts, "/"), "/")
}

func (s *Server) size(n *node) int64 {
	if !n.dir {
		return int64(len(n.data))
	}
	var total int64
	for _, c := range s.children(n.id) {
		total += s.size(c)
	}
	return total
}

// quickXor returns the base64 QuickXorHash of data.
func quickXor(data []byte) string {
	sum := quickxorhash.Sum(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}
