package graph

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jstaf/onedriver/fs/graph/mockgraph"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// mock is the fake Graph server, nil when running against a live account.
var mock *mockgraph.Server

// By default tests run against an in-memory mock of Microsoft Graph (see
// package mockgraph). Set ONEDRIVER_TEST_LIVE=1 to use a real account instead.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	if mockgraph.Live() {
		os.Chdir("../..")
	} else {
		srv := mockgraph.New()
		mock = srv
		defer srv.Close()
		GraphURL = srv.GraphURL()

		// tests expect to find ".auth_tokens.json" in the working directory
		dir, err := os.MkdirTemp("", "onedriver-graph-test-")
		if err != nil {
			fmt.Println(err)
			return 1
		}
		defer os.RemoveAll(dir)
		os.Chdir(dir)
		auth := Auth{
			AuthConfig:   AuthConfig{TokenURL: srv.TokenURL()},
			Account:      mockgraph.Account,
			AccessToken:  mockgraph.AccessToken,
			RefreshToken: mockgraph.RefreshToken,
			ExpiresAt:    time.Now().Add(24 * time.Hour).Unix(),
		}
		auth.applyDefaults()
		if err := auth.ToFile(".auth_tokens.json"); err != nil {
			fmt.Println(err)
			return 1
		}
	}

	f, _ := os.OpenFile("fusefs_tests.log", os.O_TRUNC|os.O_CREATE|os.O_RDWR, 0644)
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: f, TimeFormat: "15:04:05"})
	defer f.Close()

	// auth and log account metadata so we're extra sure who we're testing against
	auth := Authenticate(AuthConfig{}, ".auth_tokens.json", false)
	user, _ := GetUser(auth)
	drive, _ := GetDrive(auth)
	log.Info().
		Str("account", user.UserPrincipalName).
		Str("type", drive.DriveType).
		Msg("Starting tests")

	return m.Run()
}
