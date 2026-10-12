package mockgraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	errNotFound   = errors.New("itemNotFound")
	errNameExists = errors.New("nameAlreadyExists")
	errBadRequest = errors.New("invalidRequest")
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{"code": code, "message": msg},
	})
}

func writeServiceErr(w http.ResponseWriter, err error) {
	switch err {
	case errNotFound:
		writeErr(w, 404, "itemNotFound", "The resource could not be found.")
	case errNameExists:
		writeErr(w, 409, "nameAlreadyExists", "The specified item name already exists.")
	default:
		writeErr(w, 400, "invalidRequest", err.Error())
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.reqMu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.reqMu.Unlock()

	switch p := r.URL.Path; {
	case p == "/oauth2/v2.0/token":
		s.token(w, r)
	case strings.HasPrefix(p, "/upload/"):
		s.uploadChunk(w, r, strings.TrimPrefix(p, "/upload/"))
	case strings.HasPrefix(p, "/v1.0/"):
		if r.Header.Get("Authorization") != "bearer "+AccessToken {
			writeErr(w, 401, "InvalidAuthenticationToken", "Access token is empty or invalid.")
			return
		}
		s.graph(w, r, strings.TrimPrefix(p, "/v1.0"))
	default:
		writeErr(w, 404, "notFound", "unknown mock endpoint "+p)
	}
}

// token implements the OAuth2 token endpoint for both the authorization_code
// and refresh_token grants.
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": "invalid_request"})
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code", "refresh_token":
		writeJSON(w, 200, map[string]interface{}{
			"token_type":    "Bearer",
			"scope":         "user.read files.readwrite.all",
			"expires_in":    3600,
			"access_token":  AccessToken,
			"refresh_token": RefreshToken,
		})
	default:
		writeJSON(w, 400, map[string]interface{}{"error": "unsupported_grant_type"})
	}
}

// target is the result of parsing a Graph resource path.
type target struct {
	node   *node  // the addressed item, nil if it does not exist
	parent *node  // for non-existent path/name addressed items: the parent
	name   string // ... and the name of the missing item
	suffix string // "", "/children", "/content", "/createUploadSession", "/delta"
}

// parseTarget resolves the part of the URL after /me/drive/. s.mu must be held.
func (s *Server) parseTarget(rest string) target {
	switch {
	case strings.HasPrefix(rest, "root:"):
		p, suffix := rest[len("root:"):], ""
		if i := strings.LastIndex(p, ":"); i >= 0 {
			p, suffix = p[:i], p[i+1:]
		}
		p = path.Clean("/" + p)
		if n := s.resolve(p); n != nil {
			return target{node: n, suffix: suffix}
		}
		dir, name := path.Split(p)
		return target{parent: s.resolve(dir), name: name, suffix: suffix}

	case rest == "root" || strings.HasPrefix(rest, "root/"):
		return target{node: s.live(RootID), suffix: strings.TrimPrefix(rest, "root")}

	case strings.HasPrefix(rest, "items/"):
		rest = strings.TrimPrefix(rest, "items/")
		if i := strings.Index(rest, ":"); i >= 0 {
			// items/{id}:/{name}[:/suffix]
			parent := s.live(rest[:i])
			rem, suffix := rest[i+1:], ""
			if j := strings.LastIndex(rem, ":"); j >= 0 {
				rem, suffix = rem[:j], rem[j+1:]
			}
			name := strings.TrimPrefix(rem, "/")
			if parent == nil {
				return target{name: name, suffix: suffix}
			}
			if n := s.child(parent.id, name); n != nil {
				return target{node: n, suffix: suffix}
			}
			return target{parent: parent, name: name, suffix: suffix}
		}
		id, suffix := rest, ""
		if i := strings.Index(rest, "/"); i >= 0 {
			id, suffix = rest[:i], rest[i:]
		}
		return target{node: s.live(id), suffix: suffix}
	}
	return target{suffix: "?"}
}

// targetPath is the path of the item a target refers to (or would create).
func (s *Server) targetPath(t target) string {
	switch {
	case t.node != nil:
		return s.pathOf(t.node)
	case t.parent != nil:
		return path.Join(s.pathOf(t.parent), t.name)
	}
	return "/"
}

func (s *Server) graph(w http.ResponseWriter, r *http.Request, p string) {
	switch p {
	case "/me":
		writeJSON(w, 200, map[string]interface{}{"userPrincipalName": Account, "displayName": "Mock User"})
		return
	case "/me/drive":
		writeJSON(w, 200, map[string]interface{}{
			"id":        driveID,
			"driveType": "personal",
			"quota": map[string]interface{}{
				"total": 5 << 40, "used": 1 << 30, "remaining": 5<<40 - 1<<30,
				"deleted": 0, "state": "normal",
			},
		})
		return
	}
	if !strings.HasPrefix(p, "/me/drive/") {
		writeErr(w, 404, "notFound", "unknown mock endpoint "+p)
		return
	}

	isUpload := (r.Method == http.MethodPut && strings.HasSuffix(p, "/content")) ||
		(r.Method == http.MethodPost && strings.HasSuffix(p, "/createUploadSession"))
	var body []byte
	if r.Method == http.MethodPut || r.Method == http.MethodPost || r.Method == http.MethodPatch {
		body, _ = io.ReadAll(r.Body)
	}
	rest := strings.TrimPrefix(p, "/me/drive/")
	if isUpload {
		// uploads can block on PauseUploads, which must happen without the
		// state lock held
		s.mu.Lock()
		target := s.targetPath(s.parseTarget(rest))
		s.mu.Unlock()
		s.waitPaused(target)
		if status := s.failStatus(target); status != 0 {
			writeErr(w, status, "mockInjectedFailure", "upload failure injected by the test")
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.parseTarget(rest)

	switch {
	case r.Method == http.MethodGet && t.suffix == "":
		if t.node == nil {
			writeServiceErr(w, errNotFound)
			return
		}
		writeJSON(w, 200, s.json(t.node, false))

	case r.Method == http.MethodGet && t.suffix == "/children":
		s.listChildren(w, r, t)

	case r.Method == http.MethodGet && t.suffix == "/content":
		if t.node == nil || t.node.dir {
			writeServiceErr(w, errNotFound)
			return
		}
		if status := s.failDownloadStatus(s.pathOf(t.node)); status != 0 {
			writeErr(w, status, "mockInjectedFailure", "download failure injected by the test")
			return
		}
		serveContent(w, r, t.node)

	case r.Method == http.MethodGet && strings.HasPrefix(t.suffix, "/thumbnails/"):
		s.thumbnail(w, t)

	case r.Method == http.MethodGet && t.suffix == "/delta":
		s.delta(w, r, t)

	case r.Method == http.MethodPut && t.suffix == "/content":
		s.putContentReq(w, r, t, body)

	case r.Method == http.MethodPost && t.suffix == "/children":
		s.createChild(w, t, body)

	case r.Method == http.MethodPost && t.suffix == "/createUploadSession":
		s.createUploadSession(w, t, body)

	case r.Method == http.MethodPatch && t.suffix == "":
		s.patch(w, t, body)

	case r.Method == http.MethodDelete && t.suffix == "":
		if t.node == nil || t.node.id == RootID {
			writeServiceErr(w, errNotFound)
			return
		}
		s.remove(t.node)
		w.WriteHeader(204)

	default:
		writeErr(w, 405, "methodNotAllowed", fmt.Sprintf("mock does not implement %s %s", r.Method, p))
	}
}

// json renders a DriveItem. s.mu must be held.
func (s *Server) json(n *node, tombstone bool) map[string]interface{} {
	m := map[string]interface{}{
		"id":                   n.id,
		"name":                 n.name,
		"size":                 s.size(n),
		"eTag":                 fmt.Sprintf(`"{%s},%d"`, n.id, n.ver),
		"webUrl":               "https://onedrive.mock.invalid/items/" + n.id,
		"cTag":                 fmt.Sprintf(`"c:{%s},%d"`, n.id, n.cver),
		"createdDateTime":      n.created.Format(time.RFC3339),
		"lastModifiedDateTime": n.mtime.Format(time.RFC3339),
	}
	ref := map[string]interface{}{"driveId": driveID, "driveType": "personal"}
	if n.id != RootID {
		ref["id"] = n.parent
		ref["path"] = s.parentPath(n)
	}
	m["parentReference"] = ref
	if n.dir {
		count := 0
		if !n.deleted {
			count = len(s.children(n.id))
		}
		m["folder"] = map[string]interface{}{"childCount": count}
	} else {
		m["file"] = map[string]interface{}{
			"mimeType": fileMime(n.name),
			"hashes":   map[string]interface{}{"quickXorHash": quickXor(n.data)},
		}
	}
	if n.deleted {
		m["deleted"] = map[string]interface{}{"state": "deleted"}
		m["size"] = 0
	}
	return m
}

func fileMime(name string) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func (s *Server) listChildren(w http.ResponseWriter, r *http.Request, t target) {
	if t.node == nil || !t.node.dir {
		writeServiceErr(w, errNotFound)
		return
	}
	kids := s.children(t.node.id)
	skip, _ := strconv.Atoi(r.URL.Query().Get("$skiptoken"))
	if skip > len(kids) {
		skip = len(kids)
	}
	end := skip + pageSize
	if end > len(kids) {
		end = len(kids)
	}
	values := make([]map[string]interface{}, 0, end-skip)
	for _, k := range kids[skip:end] {
		values = append(values, s.json(k, false))
	}
	resp := map[string]interface{}{"value": values}
	if end < len(kids) {
		resp["@odata.nextLink"] = fmt.Sprintf("%s/me/drive/items/%s/children?$skiptoken=%d",
			s.GraphURL(), url.PathEscape(t.node.id), end)
	}
	writeJSON(w, 200, resp)
}

func serveContent(w http.ResponseWriter, r *http.Request, n *node) {
	data := n.data
	status := 200
	if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") {
		var start, end int
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil {
			start, end = 0, len(data)-1
		}
		if start >= len(data) {
			writeErr(w, 416, "requestedRangeNotSatisfiable", "range not satisfiable")
			return
		}
		if end >= len(data) {
			end = len(data) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		data = data[start : end+1]
		status = 206
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	w.Write(data)
}

func (s *Server) delta(w http.ResponseWriter, r *http.Request, t target) {
	if t.node == nil || t.node.id != RootID {
		writeErr(w, 400, "invalidRequest", "delta is only supported on the drive root")
		return
	}
	q := r.URL.Query()
	upto := s.seq
	if u := q.Get("upto"); u != "" {
		upto, _ = strconv.ParseInt(u, 10, 64)
	}
	link := func(token int64, extra string) string {
		return fmt.Sprintf("%s/me/drive/root/delta?token=%d%s", s.GraphURL(), token, extra)
	}
	var since int64
	switch tok := q.Get("token"); tok {
	case "latest":
		writeJSON(w, 200, map[string]interface{}{
			"value":            []interface{}{},
			"@odata.deltaLink": link(s.seq, ""),
		})
		return
	case "":
	default:
		since, _ = strconv.ParseInt(tok, 10, 64)
	}

	var changed []*node
	for _, n := range s.nodes {
		if n.seq > since && n.seq <= upto {
			changed = append(changed, n)
		}
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].seq < changed[j].seq })
	skip, _ := strconv.Atoi(q.Get("skip"))
	if skip > len(changed) {
		skip = len(changed)
	}
	end := skip + pageSize
	if end > len(changed) {
		end = len(changed)
	}
	values := make([]map[string]interface{}, 0)
	for _, n := range changed[skip:end] {
		values = append(values, s.json(n, n.deleted))
	}
	resp := map[string]interface{}{"value": values}
	if end < len(changed) {
		resp["@odata.nextLink"] = link(since, fmt.Sprintf("&skip=%d&upto=%d", end, upto))
	} else {
		resp["@odata.deltaLink"] = link(upto, "")
	}
	writeJSON(w, 200, resp)
}

// putContentReq implements a simple (<4MB) content upload, either to an
// existing item or to parent:/name:/content.
func (s *Server) putContentReq(w http.ResponseWriter, r *http.Request, t target, body []byte) {
	if t.node != nil {
		if t.node.dir {
			writeErr(w, 400, "invalidRequest", "cannot upload content to a folder")
			return
		}
		if r.URL.Query().Get("@microsoft.graph.conflictBehavior") == "fail" && t.name != "" {
			writeServiceErr(w, errNameExists)
			return
		}
		s.setContent(t.node, body, time.Time{})
		writeJSON(w, 200, s.json(t.node, false))
		return
	}
	if t.name == "" {
		writeServiceErr(w, errNotFound)
		return
	}
	parent := t.parent
	if parent == nil {
		writeServiceErr(w, errNotFound)
		return
	}
	n := s.putContent(parent.id, t.name, body, time.Time{})
	writeJSON(w, 201, s.json(n, false))
}

func (s *Server) createChild(w http.ResponseWriter, t target, body []byte) {
	if t.node == nil || !t.node.dir {
		writeServiceErr(w, errNotFound)
		return
	}
	var req struct {
		Name     string          `json:"name"`
		Folder   *struct{}       `json:"folder"`
		File     *struct{}       `json:"file"`
		Behavior string          `json:"@microsoft.graph.conflictBehavior"`
		Extra    json.RawMessage `json:"-"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Name == "" {
		writeErr(w, 400, "invalidRequest", "name is required")
		return
	}
	name := req.Name
	if existing := s.child(t.node.id, name); existing != nil {
		switch req.Behavior {
		case "replace":
			s.remove(existing)
		case "rename":
			for i := 1; s.child(t.node.id, name) != nil; i++ {
				ext := path.Ext(req.Name)
				name = fmt.Sprintf("%s %d%s", strings.TrimSuffix(req.Name, ext), i, ext)
			}
		default:
			writeServiceErr(w, errNameExists)
			return
		}
	}
	n := s.create(t.node.id, name, req.Folder != nil)
	writeJSON(w, 201, s.json(n, false))
}

func (s *Server) patch(w http.ResponseWriter, t target, body []byte) {
	if t.node == nil || t.node.id == RootID {
		writeServiceErr(w, errNotFound)
		return
	}
	var req struct {
		Name   string `json:"name"`
		Parent *struct {
			ID string `json:"id"`
		} `json:"parentReference"`
		Behavior    string `json:"@microsoft.graph.conflictBehavior"`
		FileSysInfo *struct {
			LastModified time.Time `json:"lastModifiedDateTime"`
		} `json:"fileSystemInfo"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, 400, "invalidRequest", err.Error())
		return
	}
	parentID := ""
	if req.Parent != nil {
		parentID = req.Parent.ID
		if parentID == "root" {
			parentID = RootID
		}
	}
	if req.Name != "" || parentID != "" {
		if _, err := s.move(t.node, req.Name, parentID, req.Behavior == "replace"); err != nil {
			writeServiceErr(w, err)
			return
		}
	}
	if req.FileSysInfo != nil && !req.FileSysInfo.LastModified.IsZero() {
		t.node.mtime = req.FileSysInfo.LastModified.UTC()
	}
	writeJSON(w, 200, s.json(t.node, false))
}

// ---- upload sessions ----

type uploadSession struct {
	path       string // target path at creation time, for PauseUploads
	existingID string // set when replacing an existing item
	parentID   string // otherwise: create parentID/name
	name       string
	mtime      time.Time
	data       []byte
}

func (s *Server) createUploadSession(w http.ResponseWriter, t target, body []byte) {
	var req struct {
		Behavior string `json:"@microsoft.graph.conflictBehavior"`
		FSInfo   struct {
			LastModified time.Time `json:"lastModifiedDateTime"`
		} `json:"fileSystemInfo"`
	}
	json.Unmarshal(body, &req)

	sess := &uploadSession{mtime: req.FSInfo.LastModified, path: s.targetPath(t)}
	switch {
	case t.node != nil && t.node.dir:
		writeErr(w, 400, "invalidRequest", "cannot upload content to a folder")
		return
	case t.node != nil:
		if req.Behavior == "fail" && t.name != "" {
			writeServiceErr(w, errNameExists)
			return
		}
		sess.existingID = t.node.id
	case t.parent != nil && t.name != "":
		sess.parentID, sess.name = t.parent.id, t.name
	default:
		writeServiceErr(w, errNotFound)
		return
	}
	id := s.newID()
	s.uploads[id] = sess
	writeJSON(w, 200, map[string]interface{}{
		"uploadUrl":          s.HTTP.URL + "/upload/" + id,
		"expirationDateTime": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	})
}

// uploadChunk handles PUT/DELETE/GET on an uploadUrl. Like the real service,
// it requires no Authorization header.
func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request, id string) {
	body, _ := io.ReadAll(r.Body)

	s.mu.Lock()
	sess := s.uploads[id]
	if sess != nil && r.Method == http.MethodPut {
		s.mu.Unlock()
		s.waitPaused(sess.path)
		if status := s.failStatus(sess.path); status != 0 {
			writeErr(w, status, "mockInjectedFailure", "upload failure injected by the test")
			return
		}
		s.mu.Lock()
		sess = s.uploads[id]
	}
	defer s.mu.Unlock()
	if sess == nil {
		writeErr(w, 404, "itemNotFound", "upload session not found")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		delete(s.uploads, id)
		w.WriteHeader(204)
		return
	case http.MethodGet:
		writeJSON(w, 200, map[string]interface{}{"nextExpectedRanges": []string{fmt.Sprintf("%d-", len(sess.data))}})
		return
	case http.MethodPut:
	default:
		writeErr(w, 405, "methodNotAllowed", "bad method for upload session")
		return
	}

	var start, end, total int
	if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil {
		writeErr(w, 400, "invalidRange", "bad Content-Range: "+r.Header.Get("Content-Range"))
		return
	}
	if start != len(sess.data) || end-start+1 != len(body) || end >= total {
		w.Header().Set("Content-Type", "application/json")
		writeErr(w, 416, "invalidRange", fmt.Sprintf(
			"expected range starting at %d, got %s with %d bytes",
			len(sess.data), r.Header.Get("Content-Range"), len(body)))
		return
	}
	sess.data = append(sess.data, body...)
	if len(sess.data) < total {
		writeJSON(w, 202, map[string]interface{}{
			"expirationDateTime": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
			"nextExpectedRanges": []string{fmt.Sprintf("%d-", len(sess.data))},
		})
		return
	}

	// final chunk: commit
	delete(s.uploads, id)
	var n *node
	status := 200
	if sess.existingID != "" {
		if n = s.live(sess.existingID); n == nil {
			writeServiceErr(w, errNotFound)
			return
		}
		s.setContent(n, sess.data, sess.mtime)
	} else {
		if s.live(sess.parentID) == nil {
			writeServiceErr(w, errNotFound)
			return
		}
		if s.child(sess.parentID, sess.name) == nil {
			status = 201
		}
		n = s.putContent(sess.parentID, sess.name, sess.data, sess.mtime)
	}
	writeJSON(w, status, s.json(n, false))
}

// thumbnail serves /thumbnails/0/c{w}x{h}/content: a generated JPEG fitting in
// the requested box for the file types OneDrive can render, 404 otherwise.
func (s *Server) thumbnail(w http.ResponseWriter, t target) {
	if t.node == nil || t.node.dir {
		writeServiceErr(w, errNotFound)
		return
	}
	var box int
	if _, err := fmt.Sscanf(t.suffix, "/thumbnails/0/c%dx%d/content", &box, &box); err != nil || box <= 0 {
		writeErr(w, 404, "itemNotFound", "no such thumbnail")
		return
	}
	switch strings.ToLower(path.Ext(t.node.name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".pdf", ".docx", ".xlsx", ".pptx", ".mp4":
	default:
		writeErr(w, 404, "itemNotFound", "no thumbnail for this file type")
		return
	}
	// 3:2 image, at most 300 pixels wide (the server may return other sizes
	// than requested)
	width := box
	if width > 300 {
		width = 300
	}
	img := image.NewRGBA(image.Rect(0, 0, width, width*2/3))
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, nil)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(200)
	w.Write(buf.Bytes())
}
