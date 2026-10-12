package graph

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGraphURLOverride(t *testing.T) {
	// Create a mock server that responds with {"id":"abc"} to GET requests
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"abc"}`))
	}))
	defer server.Close()

	// Save the original GraphURL and restore it at the end
	originalURL := GraphURL
	defer func() {
		GraphURL = originalURL
	}()

	// Override GraphURL to point to the test server
	GraphURL = server.URL

	// Create a mock Auth with a far-future ExpiresAt so it won't try to refresh
	auth := &Auth{
		AccessToken: "test-token",
		ExpiresAt:   time.Now().Add(24 * time.Hour).Unix(),
	}

	// Call Get with a simple resource path
	body, err := Get("/test", auth)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	// Assert the response body
	expected := `{"id":"abc"}`
	if string(body) != expected {
		t.Errorf("Expected body %q, got %q", expected, string(body))
	}
}
