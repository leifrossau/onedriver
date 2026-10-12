package graph

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jstaf/onedriver/fs/graph/mockgraph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenTestAuth returns an Auth with an expired access token whose token
// endpoint is a private mock server (so the tests can break it freely), while
// Graph requests go to the shared mock.
func tokenTestAuth(t *testing.T, refreshToken string) (*Auth, *mockgraph.Server) {
	t.Helper()
	if mock == nil {
		t.Skip("needs the mock Graph server (unset ONEDRIVER_TEST_LIVE)")
	}
	tok := mockgraph.New()
	t.Cleanup(tok.Close)
	a := &Auth{
		AuthConfig:   AuthConfig{TokenURL: tok.TokenURL()},
		Account:      mockgraph.Account,
		AccessToken:  mockgraph.AccessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
	}
	a.applyDefaults()
	require.NoError(t, a.ToFile(filepath.Join(t.TempDir(), "auth.json")))
	return a, tok
}

// the refresh token must be sent URL-encoded
func TestRefreshURLEncoding(t *testing.T) {
	t.Parallel()
	const refresh = "M.R3+tok/en=with&special chars%20"
	a, tok := tokenTestAuth(t, refresh)
	a.Refresh()
	forms := tok.TokenForms()
	require.Len(t, forms, 1)
	assert.Equal(t, refresh, forms[0].Get("refresh_token"))
	assert.Equal(t, "refresh_token", forms[0].Get("grant_type"))
	assert.Equal(t, a.RedirectURL, forms[0].Get("redirect_uri"))
	assert.Equal(t, a.ClientID, forms[0].Get("client_id"))
	assert.Len(t, forms[0], 4, "no extra parameters may leak out of the refresh token")
	assert.False(t, a.Expired())
	assert.Greater(t, a.ExpiryTime(), time.Now().Unix())
	// the new refresh token was saved
	var saved Auth
	require.NoError(t, saved.FromFile(a.path))
	assert.Equal(t, mockgraph.RefreshToken, saved.RefreshToken)
}

func TestAuthCodeURLEncoding(t *testing.T) {
	t.Parallel()
	_, tok := tokenTestAuth(t, "x")
	const code = "M.C5+abc/def=&x=y"
	a, err := exchangeAuthCode(AuthConfig{
		ClientID: "cid", RedirectURL: "https://example.com/?a=b&c=d", TokenURL: tok.TokenURL(),
	}, code)
	require.NoError(t, err)
	assert.NotEmpty(t, a.AccessToken)
	forms := tok.TokenForms()
	require.Len(t, forms, 1)
	assert.Equal(t, code, forms[0].Get("code"))
	assert.Equal(t, "https://example.com/?a=b&c=d", forms[0].Get("redirect_uri"))
	assert.Len(t, forms[0], 4)
}

// a failing token endpoint must not lose the login
func TestRefreshTransientFailure(t *testing.T) {
	t.Parallel()
	a, tok := tokenTestAuth(t, mockgraph.RefreshToken)
	var notified int32
	defer a.OnRenewalFailure(func(string) { atomic.AddInt32(&notified, 1) })()
	tok.SetTokenMode(mockgraph.TokenUnavailable)

	// requests keep working with the old token
	_, err := Get("/me", a)
	assert.NoError(t, err)
	assert.False(t, a.Expired())
	assert.Equal(t, mockgraph.AccessToken, a.Token())
	// ... and the token endpoint is not hammered (backoff)
	for i := 0; i < 5; i++ {
		_, err = Get("/me", a)
		assert.NoError(t, err)
	}
	assert.Len(t, tok.TokenForms(), 1)

	// after recovery and the backoff, the token is renewed
	tok.SetTokenMode(mockgraph.TokenOK)
	a.refreshMu.Lock()
	a.nextRetry = time.Time{}
	a.refreshMu.Unlock()
	_, err = Get("/me", a)
	assert.NoError(t, err)
	assert.Greater(t, a.ExpiryTime(), time.Now().Unix())
	assert.False(t, a.Expired())
	assert.EqualValues(t, 0, atomic.LoadInt32(&notified))
}

func TestRefreshInvalidGrant(t *testing.T) {
	t.Parallel()
	a, tok := tokenTestAuth(t, mockgraph.RefreshToken)
	var notified int32
	defer a.OnRenewalFailure(func(string) { atomic.AddInt32(&notified, 1) })()
	tok.SetTokenMode(mockgraph.TokenInvalidGrant)

	_, err := Get("/me", a)
	require.ErrorIs(t, err, ErrAuthExpired)
	assert.True(t, IsOffline(err), "expired logins behave like being offline")
	assert.True(t, a.Expired())
	// no further token requests, the handlers were called once
	_, err = Get("/me", a)
	require.ErrorIs(t, err, ErrAuthExpired)
	assert.Len(t, tok.TokenForms(), 1)
	assert.EqualValues(t, 1, atomic.LoadInt32(&notified))
	// tokens stay on disk for a later login to replace
	var saved Auth
	require.NoError(t, saved.FromFile(a.path))
	assert.Equal(t, mockgraph.RefreshToken, saved.RefreshToken)
}

// a 401 from Graph first tries a renewal and retries once
func TestRequest401Renews(t *testing.T) {
	t.Parallel()
	a, tok := tokenTestAuth(t, mockgraph.RefreshToken)
	a.AccessToken = "stale-token"
	a.ExpiresAt = time.Now().Add(time.Hour).Unix() // looks valid
	_, err := Get("/me", a)
	require.NoError(t, err)
	assert.Equal(t, mockgraph.AccessToken, a.Token())
	assert.Len(t, tok.TokenForms(), 1)

	// ... but if the renewal is rejected, the login is expired
	b, tok2 := tokenTestAuth(t, mockgraph.RefreshToken)
	b.AccessToken = "stale-token"
	b.ExpiresAt = time.Now().Add(time.Hour).Unix()
	tok2.SetTokenMode(mockgraph.TokenInvalidGrant)
	_, err = Get("/me", b)
	require.ErrorIs(t, err, ErrAuthExpired)
	assert.True(t, b.Expired())

	// ... and a transient renewal failure just reports the 401
	c, tok3 := tokenTestAuth(t, mockgraph.RefreshToken)
	c.AccessToken = "stale-token"
	c.ExpiresAt = time.Now().Add(time.Hour).Unix()
	tok3.SetTokenMode(mockgraph.TokenUnavailable)
	_, err = Get("/me", c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 401")
	assert.False(t, c.Expired())
}

func TestLoginReplacesTokens(t *testing.T) {
	// not parallel: changes process-wide hooks
	a, tok := tokenTestAuth(t, "old-refresh")
	tok.SetTokenMode(mockgraph.TokenInvalidGrant)
	a.Refresh()
	require.True(t, a.Expired())

	restore := SetInteractiveLogin(func(cfg AuthConfig, account string) (string, error) {
		assert.Equal(t, mockgraph.Account, account)
		return "the code", nil
	}, func() error { return nil })
	require.NoError(t, a.Login())
	restore()
	assert.False(t, a.Expired())
	assert.Equal(t, mockgraph.RefreshToken, a.refreshTokenValue())
	assert.Greater(t, a.ExpiryTime(), time.Now().Unix())
	var saved Auth
	require.NoError(t, saved.FromFile(a.path))
	assert.Equal(t, mockgraph.RefreshToken, saved.RefreshToken)

	// a cancelled login changes nothing
	a.MarkExpired()
	defer SetInteractiveLogin(func(AuthConfig, string) (string, error) {
		return "", errors.New("window closed")
	}, nil)()
	assert.Error(t, a.Login())
	assert.True(t, a.Expired())
}

func TestLoginUnavailableHeadless(t *testing.T) {
	// not parallel: changes process-wide state
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	err := LoginUnavailable()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "onedriver -n")
}
