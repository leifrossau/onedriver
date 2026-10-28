package graph

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFixNegativeSizes(t *testing.T) {
	in := `{"value":[{"id":"a","size":-691887,"name":"x"},{"id":"b","size" : -1.5e3},` +
		`{"id":"c","size":42,"name":"has \"size\":-1 in it"}]}`
	out := fixNegativeSizes([]byte(in))
	var page struct {
		Value []DriveItem `json:"value"`
	}
	require.NoError(t, json.Unmarshal(out, &page))
	require.Len(t, page.Value, 3)
	assert.Equal(t, uint64(0), page.Value[0].Size)
	assert.Equal(t, uint64(0), page.Value[1].Size)
	assert.Equal(t, uint64(42), page.Value[2].Size)
	assert.Equal(t, `has "size":-1 in it`, page.Value[2].Name, "names must not be touched")
	assert.Equal(t, `{"size":1}`, string(fixNegativeSizes([]byte(`{"size":1}`))))
}

// Only JSON responses are rewritten: file content that happens to contain
// the pattern must come back unchanged.
func TestRequestFixesNegativeSizesOnlyInJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Write([]byte(`{"id":"a","size":-5}`))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte(`{"size":-5}`))
	}))
	defer srv.Close()
	old := GraphURL
	GraphURL = srv.URL
	defer func() { GraphURL = old }()
	auth := &Auth{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}

	body, err := Get("/json", auth)
	require.NoError(t, err)
	var item DriveItem
	require.NoError(t, json.Unmarshal(body, &item), string(body))
	assert.Equal(t, uint64(0), item.Size)

	body, err = Get("/content", auth)
	require.NoError(t, err)
	assert.Equal(t, `{"size":-5}`, string(body))
}

func TestTrimRootPrefix(t *testing.T) {
	assert.Equal(t, "/Test", TrimRootPrefix("/drive/root:/Test"))
	assert.Equal(t, "/Test", TrimRootPrefix("/drives/73B92D8981ACF028/root:/Test"))
	assert.Equal(t, "", TrimRootPrefix("/drives/73B92D8981ACF028/root:"))
	assert.Equal(t, "", TrimRootPrefix("/drive/root:"))
}
