package mockgraph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type item struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ETag    string `json:"eTag"`
	Deleted *struct {
		State string `json:"state"`
	} `json:"deleted"`
	Parent struct {
		ID string `json:"id"`
	} `json:"parentReference"`
	File *struct {
		Hashes struct {
			QuickXorHash string `json:"quickXorHash"`
		} `json:"hashes"`
	} `json:"file"`
	Folder *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
}

type page struct {
	Value     []item `json:"value"`
	NextLink  string `json:"@odata.nextLink"`
	DeltaLink string `json:"@odata.deltaLink"`
}

func call(t *testing.T, s *Server, method, url string, body []byte, hdr ...string) (int, []byte) {
	t.Helper()
	if !strings.HasPrefix(url, "http") {
		url = s.GraphURL() + url
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	require.NoError(t, err)
	if !strings.Contains(url, "/upload/") {
		req.Header.Set("Authorization", "bearer "+AccessToken)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func decode(t *testing.T, b []byte, v interface{}) {
	t.Helper()
	require.NoError(t, json.Unmarshal(b, v), string(b))
}

func TestAuthRequired(t *testing.T) {
	s := New()
	defer s.Close()
	resp, err := http.Get(s.GraphURL() + "/me/drive/root")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 401, resp.StatusCode)
}

func TestItemLookups(t *testing.T) {
	s := New()
	defer s.Close()
	id := s.PutFile("/a/b/file.txt", []byte("hello"))

	for _, url := range []string{
		"/me/drive/items/" + id,
		"/me/drive/root:%2Fa%2Fb%2Ffile.txt",
		"/me/drive/root:/A/B/FILE.TXT", // case-insensitive
		"/me/drive/items/" + s.ID("/a/b") + ":/file.txt",
	} {
		code, body := call(t, s, "GET", url, nil)
		require.Equal(t, 200, code, url)
		var it item
		decode(t, body, &it)
		assert.Equal(t, id, it.ID, url)
		assert.Equal(t, int64(5), it.Size)
		require.NotNil(t, it.File)
		assert.NotEmpty(t, it.File.Hashes.QuickXorHash)
	}

	code, _ := call(t, s, "GET", "/me/drive/root:/a/nope", nil)
	assert.Equal(t, 404, code)

	// the root, by alias and by its real ID
	for _, url := range []string{"/me/drive/root", "/me/drive/items/" + RootID} {
		code, body := call(t, s, "GET", url, nil)
		require.Equal(t, 200, code)
		var it item
		decode(t, body, &it)
		assert.Equal(t, RootID, it.ID)
		assert.Equal(t, "root", it.Name)
		assert.NotNil(t, it.Folder)
	}
}

func TestChildrenPaging(t *testing.T) {
	s := New()
	defer s.Close()
	for i := 0; i < 450; i++ {
		s.PutFile(fmt.Sprintf("/many/%d.txt", i), []byte("x"))
	}
	url := "/me/drive/root:/many:/children"
	var names []string
	pages := 0
	for url != "" {
		code, body := call(t, s, "GET", url, nil)
		require.Equal(t, 200, code)
		var p page
		decode(t, body, &p)
		for _, it := range p.Value {
			names = append(names, it.Name)
		}
		url = strings.TrimPrefix(p.NextLink, s.GraphURL())
		pages++
	}
	assert.Equal(t, 3, pages)
	assert.Len(t, names, 450)
}

func TestMkdirConflicts(t *testing.T) {
	s := New()
	defer s.Close()
	code, body := call(t, s, "POST", "/me/drive/root/children", []byte(`{"name":"dir","folder":{}}`))
	require.Equal(t, 201, code, string(body))
	code, body = call(t, s, "POST", "/me/drive/root/children", []byte(`{"name":"DIR","folder":{}}`))
	assert.Equal(t, 409, code)
	assert.Contains(t, string(body), "nameAlreadyExists")
	code, body = call(t, s, "POST", "/me/drive/root/children",
		[]byte(`{"name":"dir","folder":{},"@microsoft.graph.conflictBehavior":"rename"}`))
	require.Equal(t, 201, code)
	var it item
	decode(t, body, &it)
	assert.Equal(t, "dir 1", it.Name)
}

func TestPatchRenameMove(t *testing.T) {
	s := New()
	defer s.Close()
	a := s.PutFile("/src/a.txt", []byte("A"))
	s.PutFile("/dst/a.txt", []byte("old"))
	dst := s.ID("/dst")

	body := fmt.Sprintf(`{"name":"a.txt","parentReference":{"id":%q}}`, dst)
	code, resp := call(t, s, "PATCH", "/me/drive/items/"+a, []byte(body))
	assert.Equal(t, 409, code, string(resp))

	body = fmt.Sprintf(`{"name":"a.txt","parentReference":{"id":%q},"@microsoft.graph.conflictBehavior":"replace"}`, dst)
	code, resp = call(t, s, "PATCH", "/me/drive/items/"+a, []byte(body))
	require.Equal(t, 200, code, string(resp))
	got, _ := s.ContentPath("/dst/a.txt")
	assert.Equal(t, "A", string(got))
	assert.False(t, s.Exists("/src/a.txt"))
}

func TestContentRangeAndSimpleUpload(t *testing.T) {
	s := New()
	defer s.Close()
	parent := s.Mkdir("/up")
	code, body := call(t, s, "PUT", "/me/drive/items/"+parent+":/new.txt:/content", []byte("0123456789"))
	require.Equal(t, 201, code, string(body))
	var it item
	decode(t, body, &it)

	code, body = call(t, s, "GET", "/me/drive/items/"+it.ID+"/content", nil, "Range", "bytes=2-4")
	assert.Equal(t, 206, code)
	assert.Equal(t, "234", string(body))

	code, _ = call(t, s, "PUT", "/me/drive/items/"+it.ID+"/content", []byte("replaced"))
	assert.Equal(t, 200, code)
	got, _ := s.Content(it.ID)
	assert.Equal(t, "replaced", string(got))
}

func TestContentDownloadStats(t *testing.T) {
	s := New()
	defer s.Close()
	id := s.PutFile("/dl/a.bin", []byte("0123456789"))

	code, body := call(t, s, "GET", "/me/drive/items/"+id+"/content", nil, "Range", "bytes=5-")
	assert.Equal(t, 206, code)
	assert.Equal(t, "56789", string(body))
	code, body = call(t, s, "GET", "/me/drive/items/"+id+"/content", nil, "Range", "bytes=8-20")
	assert.Equal(t, 206, code)
	assert.Equal(t, "89", string(body))
	code, _ = call(t, s, "GET", "/me/drive/items/"+id+"/content", nil, "Range", "bytes=10-12")
	assert.Equal(t, 416, code)
	assert.EqualValues(t, 7, s.DownloadedBytes(id))
	assert.Equal(t, []string{"bytes=5-", "bytes=8-20"}, s.ContentRanges(id))

	s.CorruptDownloads(id, 1)
	_, body = call(t, s, "GET", "/me/drive/items/"+id+"/content", nil, "Range", "bytes=0-3")
	assert.Equal(t, []byte{'0' ^ 0xff, '1', '2', '3'}, body)
	_, body = call(t, s, "GET", "/me/drive/items/"+id+"/content", nil, "Range", "bytes=0-3")
	assert.Equal(t, "0123", string(body), "only the next response is corrupted")
	data, _ := s.Content(id)
	assert.Equal(t, "0123456789", string(data))
}

func TestUploadSession(t *testing.T) {
	s := New()
	defer s.Close()
	parent := s.Mkdir("/big")
	code, body := call(t, s, "POST", "/me/drive/items/"+parent+":/file.bin:/createUploadSession",
		[]byte(`{"@microsoft.graph.conflictBehavior":"replace","fileSystemInfo":{"lastModifiedDateTime":"2020-01-02T03:04:05Z"}}`))
	require.Equal(t, 200, code, string(body))
	var sess struct {
		UploadURL string `json:"uploadUrl"`
	}
	decode(t, body, &sess)

	data := []byte("0123456789abcdef")
	// out-of-order chunks are rejected
	code, _ = call(t, s, "PUT", sess.UploadURL, data[8:], "Content-Range", "bytes 8-15/16")
	assert.Equal(t, 416, code)
	code, body = call(t, s, "PUT", sess.UploadURL, data[:8], "Content-Range", "bytes 0-7/16")
	assert.Equal(t, 202, code, string(body))
	assert.False(t, s.Exists("/big/file.bin"), "must not exist before the last chunk")
	code, body = call(t, s, "PUT", sess.UploadURL, data[8:], "Content-Range", "bytes 8-15/16")
	require.Equal(t, 201, code, string(body))
	var it item
	decode(t, body, &it)
	assert.Equal(t, int64(16), it.Size)
	got, _ := s.ContentPath("/big/file.bin")
	assert.Equal(t, data, got)
}

func TestDelta(t *testing.T) {
	s := New()
	defer s.Close()
	s.PutFile("/before.txt", []byte("1"))

	fetch := func(url string) (items []item, deltaLink string) {
		for url != "" {
			code, body := call(t, s, "GET", url, nil)
			require.Equal(t, 200, code, string(body))
			var p page
			decode(t, body, &p)
			items = append(items, p.Value...)
			if p.DeltaLink != "" {
				deltaLink = strings.TrimPrefix(p.DeltaLink, s.GraphURL())
			}
			url = strings.TrimPrefix(p.NextLink, s.GraphURL())
		}
		return
	}

	items, link := fetch("/me/drive/root/delta?token=latest")
	assert.Empty(t, items)
	require.NotEmpty(t, link)

	items, link2 := fetch(link)
	assert.Empty(t, items, "nothing changed")

	id := s.PutFile("/dir/new.txt", []byte("hello"))
	s.Modify(s.ID("/before.txt"), []byte("2"))
	gone := s.PutFile("/gone.txt", []byte("bye"))
	_, link3 := fetch(link2)
	s.Delete(gone)

	items, _ = fetch(link3)
	byID := map[string]item{}
	for _, it := range items {
		byID[it.ID] = it
	}
	assert.Contains(t, byID, gone)
	assert.NotNil(t, byID[gone].Deleted)
	assert.NotEmpty(t, byID[gone].Parent.ID, "tombstones need a parentReference.id")
	assert.Contains(t, byID, RootID, "parent folders show up when children change")
	assert.Empty(t, byID[RootID].Parent.ID, "root has no parent id")

	// a fresh listing from the start of time contains everything live
	items, _ = fetch("/me/drive/root/delta")
	assert.Contains(t, itemIDs(items), id)
}

func itemIDs(items []item) []string {
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

func TestDeltaPaging(t *testing.T) {
	s := New()
	defer s.Close()
	_, body := call(t, s, "GET", "/me/drive/root/delta?token=latest", nil)
	var p page
	decode(t, body, &p)
	link := strings.TrimPrefix(p.DeltaLink, s.GraphURL())
	for i := 0; i < 450; i++ {
		s.PutFile(fmt.Sprintf("/d/%d", i), nil)
	}
	total, pages := 0, 0
	for link != "" {
		_, body = call(t, s, "GET", link, nil)
		p = page{}
		decode(t, body, &p)
		total += len(p.Value)
		pages++
		if p.NextLink != "" {
			link = strings.TrimPrefix(p.NextLink, s.GraphURL())
		} else {
			break
		}
	}
	assert.Equal(t, 3, pages)
	assert.Equal(t, 450+2, total, "450 files plus the /d and root folders")
}

func TestPauseUploadsIsScoped(t *testing.T) {
	s := New()
	defer s.Close()
	s.Mkdir("/held")
	s.Mkdir("/free")
	s.PauseUploads("/held")

	done := make(chan int, 1)
	go func() {
		code, _ := call(t, s, "PUT", "/me/drive/root:/held/x.txt:/content", []byte("x"))
		done <- code
	}()
	code, _ := call(t, s, "PUT", "/me/drive/root:/free/x.txt:/content", []byte("x"))
	assert.Equal(t, 201, code)
	select {
	case <-done:
		t.Fatal("paused upload completed")
	default:
	}
	assert.False(t, s.Exists("/held/x.txt"))
	s.ResumeUploads("/held")
	assert.Equal(t, 201, <-done)
	assert.True(t, s.Exists("/held/x.txt"))
}

func TestTokenEndpoint(t *testing.T) {
	s := New()
	defer s.Close()
	resp, err := http.Post(s.TokenURL(), "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=refresh_token&refresh_token=x"))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	decode(t, body, &tok)
	assert.Equal(t, AccessToken, tok.AccessToken)
	assert.Equal(t, RefreshToken, tok.RefreshToken)
	assert.Greater(t, tok.ExpiresIn, 0)
}
