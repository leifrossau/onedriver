package fs

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThumbnailURI(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "file:///home/me/OneDrive/a.png",
		thumbnailURI("/home/me/OneDrive", "/a.png"))
	assert.Equal(t, "file:///home/me/OneDrive/My%20Docs/Ansh%C3%B8rning%20(1).docx",
		thumbnailURI("/home/me/OneDrive", "/My Docs/Anshørning (1).docx"))
	assert.Equal(t, "file:///m/100%25%20%23a%3Fb%5B1%5D.jpg",
		thumbnailURI("/m", "/100% #a?b[1].jpg"))
	// sub-delimiters stay as they are
	assert.Equal(t, "file:///m/a&b=c,d;e+f'g!h$i@j:k.png",
		thumbnailURI("/m", "/a&b=c,d;e+f'g!h$i@j:k.png"))
}

func TestThumbnailName(t *testing.T) {
	t.Parallel()
	uri := "file:///home/me/OneDrive/Ansh%C3%B8rning%20(1).docx"
	sum := md5.Sum([]byte(uri))
	assert.Equal(t, hex.EncodeToString(sum[:])+".png", thumbnailName(uri))
	assert.Len(t, thumbnailName(uri), 36)
}

func testImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 7, 255})
		}
	}
	return img
}

func TestPNGTextChunks(t *testing.T) {
	t.Parallel()
	text := []textChunk{
		{"Thumb::URI", "file:///x/Ansh%C3%B8rning.png"},
		{"Thumb::MTime", "1700000000"},
		{"Thumb::Size", "1234"},
		{"Software", "onedriver"},
	}
	src := testImage(40, 20)
	data, err := encodePNGWithText(src, text)
	require.NoError(t, err)

	// still a valid PNG with identical pixels
	dec, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, src.Bounds(), dec.Bounds())
	r1, g1, b1, _ := src.At(13, 5).RGBA()
	r2, g2, b2, _ := dec.At(13, 5).RGBA()
	assert.Equal(t, []uint32{r1, g1, b1}, []uint32{r2, g2, b2})

	got, err := readPNGText(data)
	require.NoError(t, err)
	for _, c := range text {
		assert.Equal(t, c.value, got[c.key], c.key)
	}
	// the chunks sit directly after IHDR
	assert.Equal(t, "tEXt", string(data[8+25+4:8+25+8]))

	_, err = readPNGText([]byte("nope"))
	assert.Error(t, err)
}

func TestScaleToFit(t *testing.T) {
	t.Parallel()
	assert.Equal(t, image.Rect(0, 0, 128, 64), scaleToFit(testImage(512, 256), 128).Bounds())
	assert.Equal(t, image.Rect(0, 0, 64, 128), scaleToFit(testImage(256, 512), 128).Bounds())
	// never enlarges
	assert.Equal(t, image.Rect(0, 0, 100, 50), scaleToFit(testImage(100, 50), 256).Bounds())
}

func TestThumbnailValidMTime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "normal", "x.png")
	uri := "file:///m/a.png"
	data, err := encodePNGWithText(testImage(4, 4), []textChunk{
		{"Thumb::URI", uri}, {"Thumb::MTime", "1700000000"}, {"Thumb::Size", "99"},
	})
	require.NoError(t, err)
	require.NoError(t, writeFileAtomic(p, data))

	assert.True(t, thumbnailValid(p, uri, 1700000000, 99))
	assert.False(t, thumbnailValid(p, uri, 1700000001, 99), "mtime changed")
	assert.False(t, thumbnailValid(p, uri, 1700000000, 100), "size changed")
	assert.False(t, thumbnailValid(p, "file:///m/b.png", 1700000000, 99), "other URI")
	assert.False(t, thumbnailValid(p+".missing", uri, 1700000000, 99))

	st, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), st.Mode().Perm())
	st, err = os.Stat(filepath.Dir(p))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0700), st.Mode().Perm())
	entries, _ := os.ReadDir(filepath.Dir(p))
	assert.Len(t, entries, 1, "no temp files left behind")
}

// Listing a folder fills the thumbnail cache from the server's thumbnails
// without ever downloading the files.
func TestE2EServerThumbnails(t *testing.T) {
	t.Parallel()
	const remoteDir = "/e2e_thumbs"
	dir, cache := mountMockFS(t, remoteDir)
	mnt := filepath.Dir(dir)
	cacheHome := t.TempDir()
	cache.SetThumbnails(ThumbnailOptions{
		Enabled:     true,
		Mountpoint:  mnt,
		CacheHome:   cacheHome,
		MinInterval: time.Millisecond,
	})

	imgID := mock.PutFile(remoteDir+"/Ansøgning foto.jpg", []byte("not really a jpeg"))
	txtID := mock.PutFile(remoteDir+"/notes.txt", []byte("plain text"))
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "Ansøgning foto.jpg"))
		return err == nil
	}, 10*time.Second, 100*time.Millisecond)

	// list the folder, like a file manager does
	_, err := os.ReadDir(dir)
	require.NoError(t, err)
	st, err := os.Stat(filepath.Join(dir, "Ansøgning foto.jpg"))
	require.NoError(t, err)

	uri := thumbnailURI(mnt, remoteDir+"/Ansøgning foto.jpg")
	assert.True(t, strings.HasSuffix(uri, "/e2e_thumbs/Ans%C3%B8gning%20foto.jpg"), uri)
	var paths []string
	for _, tier := range []string{"normal", "large", "x-large"} {
		paths = append(paths, filepath.Join(cacheHome, "thumbnails", tier, thumbnailName(uri)))
	}
	require.Eventually(t, func() bool {
		for _, p := range paths {
			if !thumbnailValid(p, uri, uint64(st.ModTime().Unix()), uint64(st.Size())) {
				return false
			}
		}
		return true
	}, 10*time.Second, 50*time.Millisecond)

	for i, want := range []int{128, 256, 512} {
		data, err := os.ReadFile(paths[i])
		require.NoError(t, err)
		img, err := png.Decode(bytes.NewReader(data))
		require.NoError(t, err)
		longest := img.Bounds().Dx()
		assert.LessOrEqual(t, longest, want)
		text, _ := readPNGText(data)
		assert.Equal(t, uri, text["Thumb::URI"])
		assert.Equal(t, strconv.FormatInt(st.ModTime().Unix(), 10), text["Thumb::MTime"])
	}

	// nothing for the text file, nothing in fail/, and no file was downloaded
	_, err = os.Stat(filepath.Join(cacheHome, "thumbnails", "fail"))
	assert.True(t, os.IsNotExist(err))
	var thumbRequests int
	for _, r := range mock.Requests() {
		if strings.Contains(r, "/items/"+imgID+"/") {
			if strings.Contains(r, "/thumbnails/0/c512x512/content") {
				thumbRequests++
			} else {
				assert.Failf(t, "unexpected request for the image", "%s", r)
			}
		}
		assert.NotContains(t, r, "/items/"+txtID+"/")
	}
	assert.Equal(t, 1, thumbRequests, "one server request serves all three sizes")

	// a second listing does not fetch again
	_, err = os.ReadDir(dir)
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
	var after int
	for _, r := range mock.Requests() {
		if strings.Contains(r, "/items/"+imgID+"/thumbnails/0/c512x512/content") {
			after++
		}
	}
	assert.Equal(t, thumbRequests, after)
}
