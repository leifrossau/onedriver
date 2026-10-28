// Package graph provides the basic APIs to interact with Microsoft Graph. This includes
// the DriveItem resource and supporting resources which are the basis of working with
// files and folders through the Microsoft Graph API.
package graph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// GraphURL is the API endpoint of Microsoft Graph. Tests may override this
// variable to point at an httptest server for testing purposes.
var GraphURL = "https://graph.microsoft.com/v1.0"

func init() {
	if u := os.Getenv("ONEDRIVER_GRAPH_URL"); u != "" {
		GraphURL = u
	}
}

// graphError is an internal struct used when decoding Graph's error messages
type graphError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// This is an additional header that can be specified to Request
type Header struct {
	key, value string
}

// Request performs an authenticated request to Microsoft Graph. While the login
// is expired it returns ErrAuthExpired without contacting the server.
func Request(resource string, auth *Auth, method string, content io.Reader, headers ...Header) ([]byte, error) {
	if auth == nil || auth.Token() == "" {
		if auth != nil && auth.Expired() {
			return nil, ErrAuthExpired
		}
		// a catch all condition to avoid wiping our auth by accident
		log.Error().Msg("Auth was empty and we attempted to make a request with it!")
		return nil, errors.New("cannot make request with empty auth")
	}

	// transient renewal failures are not fatal: the old token may still work
	if err := auth.refresh(""); errors.Is(err, ErrAuthExpired) {
		return nil, err
	}

	client := &http.Client{Timeout: 60 * time.Second}
	token := auth.Token()
	newRequest := func() *http.Request {
		request, _ := http.NewRequest(method, GraphURL+resource, content)
		request.Header.Add("Authorization", "bearer "+token)
		switch method { // request type-specific code here
		case "PATCH":
			request.Header.Add("If-Match", "*")
			request.Header.Add("Content-Type", "application/json")
		case "POST":
			request.Header.Add("Content-Type", "application/json")
		case "PUT":
			request.Header.Add("Content-Type", "text/plain")
		}
		for _, header := range headers {
			request.Header.Add(header.key, header.value)
		}
		return request
	}
	// rewind the body for a second attempt, if that is possible
	rewind := func() {
		if seeker, ok := content.(io.Seeker); ok {
			seeker.Seek(0, io.SeekStart)
		}
	}

	request := newRequest()
	response, err := client.Do(request)
	if err != nil {
		// The actual request failed. Connections kept alive from before a
		// suspend or a network change are dead, make sure the next attempt
		// opens new ones instead of hanging on them.
		http.DefaultClient.CloseIdleConnections()
		return nil, err
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()

	if response.StatusCode == 401 {
		var gerr graphError
		json.Unmarshal(body, &gerr)
		log.Warn().
			Str("code", gerr.Error.Code).
			Str("message", gerr.Error.Message).
			Msg("Graph rejected the access token, trying to renew it.")

		// Never log in interactively from here. Renew the tokens and try
		// again once; if the refresh token is rejected, the login is expired.
		switch rerr := auth.refresh(token); {
		case errors.Is(rerr, ErrAuthExpired):
			return nil, rerr
		case rerr == nil:
			token = auth.Token()
			rewind()
			request = newRequest()
			response, err = client.Do(request)
			if err != nil {
				return nil, err
			}
			body, _ = io.ReadAll(response.Body)
			response.Body.Close()
		}
		// otherwise the renewal failed transiently, report the 401 below
	} else if response.StatusCode >= 500 {
		// the onedrive API is having issues, retry once
		rewind()
		request = newRequest()
		response, err = client.Do(request)
		if err != nil {
			return nil, err
		}
		body, _ = io.ReadAll(response.Body)
		response.Body.Close()
	}

	if response.StatusCode >= 400 {
		// something was wrong with the request
		var gerr graphError
		json.Unmarshal(body, &gerr)
		return nil, fmt.Errorf("HTTP %d - %s: %s",
			response.StatusCode, gerr.Error.Code, gerr.Error.Message)
	}
	if strings.Contains(response.Header.Get("Content-Type"), "json") {
		body = fixNegativeSizes(body)
	}
	return body, nil
}

// negativeSize matches a negative "size" member in a JSON document. Inside a
// JSON string the quotes would be escaped (\"size\"), so names can't match.
var negativeSize = regexp.MustCompile(`"size"(\s*):(\s*)-[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?`)

// fixNegativeSizes replaces negative item sizes with 0. OneDrive occasionally
// reports one (seen on a personal account: -691887); DriveItem.Size is a
// uint64, so the whole response - e.g. a delta page - would fail to decode
// and no later change could be applied until that item changed again.
func fixNegativeSizes(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"size"`)) {
		return body
	}
	return negativeSize.ReplaceAll(body, []byte(`"size"${1}:${2}0`))
}

// Get is a convenience wrapper around Request
func Get(resource string, auth *Auth, headers ...Header) ([]byte, error) {
	return Request(resource, auth, "GET", nil, headers...)
}

// Patch is a convenience wrapper around Request
func Patch(resource string, auth *Auth, content io.Reader, headers ...Header) ([]byte, error) {
	return Request(resource, auth, "PATCH", content, headers...)
}

// Post is a convenience wrapper around Request
func Post(resource string, auth *Auth, content io.Reader, headers ...Header) ([]byte, error) {
	return Request(resource, auth, "POST", content, headers...)
}

// Put is a convenience wrapper around Request
func Put(resource string, auth *Auth, content io.Reader, headers ...Header) ([]byte, error) {
	return Request(resource, auth, "PUT", content, headers...)
}

// Delete performs an HTTP delete
func Delete(resource string, auth *Auth, headers ...Header) error {
	_, err := Request(resource, auth, "DELETE", nil, headers...)
	return err
}

// IDPath computes the resource path for an item by ID
func IDPath(id string) string {
	if id == "root" {
		return "/me/drive/root"
	}
	return "/me/drive/items/" + url.PathEscape(id)
}

// ResourcePath translates an item's path to the proper path used by Graph
func ResourcePath(path string) string {
	if path == "/" {
		return "/me/drive/root"
	}
	return "/me/drive/root:" + url.PathEscape(path)
}

// ChildrenPath returns the path to an item's children
func childrenPath(path string) string {
	if path == "/" {
		return ResourcePath(path) + "/children"
	}
	return ResourcePath(path) + ":/children"
}

// ChildrenPathID returns the API resource path of an item's children
func childrenPathID(id string) string {
	return fmt.Sprintf("/me/drive/items/%s/children", url.PathEscape(id))
}

// User represents the user. Currently only used to fetch the account email so
// we can display it in file managers with .xdg-volume-info
// https://docs.microsoft.com/en-ca/graph/api/user-get
type User struct {
	UserPrincipalName string `json:"userPrincipalName"`
}

// GetUser fetches the current user details from the Graph API.
func GetUser(auth *Auth) (User, error) {
	resp, err := Get("/me", auth)
	user := User{}
	if err == nil {
		err = json.Unmarshal(resp, &user)
	}
	return user, err
}

// DriveQuota is used to parse the User's current storage quotas from the API
// https://docs.microsoft.com/en-us/onedrive/developer/rest-api/resources/quota
type DriveQuota struct {
	Deleted   uint64 `json:"deleted"`   // bytes in recycle bin
	FileCount uint64 `json:"fileCount"` // unavailable on personal accounts
	Remaining uint64 `json:"remaining"`
	State     string `json:"state"` // normal | nearing | critical | exceeded
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
}

// Drive has some general information about the user's OneDrive
// https://docs.microsoft.com/en-us/onedrive/developer/rest-api/resources/drive
type Drive struct {
	ID        string     `json:"id"`
	DriveType string     `json:"driveType"` // personal | business | documentLibrary
	Quota     DriveQuota `json:"quota,omitempty"`
}

// GetDrive is used to fetch the details of the user's OneDrive.
func GetDrive(auth *Auth) (Drive, error) {
	resp, err := Get("/me/drive", auth)
	drive := Drive{}
	if err != nil {
		return drive, err
	}
	return drive, json.Unmarshal(resp, &drive)
}

// IsResyncRequired reports whether err says the server no longer accepts a
// delta token (410 Gone, "resyncRequired"), so the sync has to start over.
func IsResyncRequired(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "HTTP 410 - ")
}

// IsOffline checks if an error string from Request() is indicative of being offline.
func IsOffline(err error) bool {
	if err == nil {
		return false
	}
	// our error messages from Request() will be prefixed with "HTTP ### -" if we actually
	// got an HTTP response (indicating we are not offline)
	rexp := regexp.MustCompile("HTTP [0-9]+ - ")
	return !rexp.MatchString(err.Error())
}
