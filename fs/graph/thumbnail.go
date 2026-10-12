package graph

import (
	"errors"
	"fmt"
	"strings"
)

// ThumbnailSize is the bounding box (in pixels) of the largest server-side
// thumbnail that onedriver asks for.
const ThumbnailSize = 512

// ErrNoThumbnail is returned by GetItemThumbnail when the server has no
// thumbnail for the item (for example, for unsupported file types).
var ErrNoThumbnail = errors.New("item has no thumbnail")

// GetItemThumbnail downloads the server-side thumbnail of an item, an image
// (usually JPEG) that fits in a size x size pixel box with the original aspect
// ratio ("c{w}x{h}" custom size). The server may return a different size than
// requested. The content endpoint answers with a redirect to the image.
// https://learn.microsoft.com/en-us/graph/api/driveitem-list-thumbnails
func GetItemThumbnail(id string, size int, auth *Auth) ([]byte, error) {
	body, err := Get(fmt.Sprintf("%s/thumbnails/0/c%dx%d/content", IDPath(id), size, size), auth)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return nil, ErrNoThumbnail
		}
		return nil, err
	}
	if len(body) == 0 {
		return nil, ErrNoThumbnail
	}
	return body, nil
}
