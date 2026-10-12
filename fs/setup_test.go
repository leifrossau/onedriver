package fs

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/jstaf/onedriver/fs/graph"
	"github.com/jstaf/onedriver/fs/graph/mockgraph"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	mountLoc     = "mount"
	testDBLoc    = "tmp"
	TestDir      = mountLoc + "/onedriver_tests"
	DeltaDir     = TestDir + "/delta"
	retrySeconds = 60 * time.Second //lint:ignore ST1011 a
)

var (
	auth *graph.Auth
	fs   *Filesystem

	// mock is the fake Microsoft Graph server the tests run against, unless
	// ONEDRIVER_TEST_LIVE=1 is set (in which case it is nil).
	mock *mockgraph.Server
)

// newMockAuth returns credentials the mock server accepts. They expire far in
// the future so no token refresh (or login window) is ever attempted.
func newMockAuth(srv *mockgraph.Server) *graph.Auth {
	return &graph.Auth{
		AuthConfig:   graph.AuthConfig{TokenURL: srv.TokenURL()},
		Account:      mockgraph.Account,
		AccessToken:  mockgraph.AccessToken,
		RefreshToken: mockgraph.RefreshToken,
		ExpiresAt:    time.Now().Add(10 * 365 * 24 * time.Hour).Unix(),
	}
}

// writeFastaFixture creates a synthetic stand-in for dmel.fa (a ~12MB FASTA
// file, which is larger than one upload chunk) with the same header and final
// line the real file has.
func writeFastaFixture(name string) error {
	const lineLen = 60
	rng := rand.New(rand.NewSource(1))
	var buf bytes.Buffer
	buf.WriteString(">X dna:chromosome chromosome:BDGP6.22:X:1:23542271:1 REF\n")
	line := make([]byte, lineLen+1)
	line[lineLen] = '\n'
	for buf.Len() < 12*1024*1024 {
		for i := 0; i < lineLen; i++ {
			line[i] = "ACGT"[rng.Intn(4)]
		}
		buf.Write(line)
	}
	buf.WriteString("AAATAAAATAC\n")
	return os.WriteFile(name, buf.Bytes(), 0644)
}

// Tests are done in the main project directory with a mounted filesystem to
// avoid having to repeatedly recreate auth_tokens.json and juggle multiple auth
// sessions.
func TestMain(m *testing.M) {
	if os.Getenv(runnerEnv) == "1" {
		runnerMain()
		os.Exit(0)
	}
	// determine if we're running a single test in vscode or something
	var singleTest bool
	for _, arg := range os.Args {
		if strings.Contains(arg, "-test.run") {
			singleTest = true
		}
	}

	live := mockgraph.Live()
	var workDir string
	if live {
		os.Chdir("..")
	} else {
		// Run in a scratch directory so the mount, caches and logs never touch
		// the source tree.
		mock = mockgraph.New()
		defer mock.Close()
		graph.GraphURL = mock.GraphURL()
		var err error
		workDir, err = os.MkdirTemp("", "onedriver-fs-test-")
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		os.Chdir(workDir)
		if err := writeFastaFixture("dmel.fa"); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	}
	// attempt to unmount regardless of what happens (in case previous tests
	// failed and didn't clean themselves up)
	// helper process for running external commands; must start before anything is mounted
	if err := startRunner(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	runErr("fusermount3", "-uz", mountLoc)
	os.Mkdir(mountLoc, 0755)
	// wipe all cached data from previous tests
	os.RemoveAll(testDBLoc)
	os.Mkdir(testDBLoc, 0755)

	f, _ := os.OpenFile("fusefs_tests.log", os.O_TRUNC|os.O_CREATE|os.O_RDWR, 0644)
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: f, TimeFormat: "15:04:05"})
	defer f.Close()

	if live {
		auth = graph.Authenticate(graph.AuthConfig{}, ".auth_tokens.json", false)
	} else {
		auth = newMockAuth(mock)
	}
	fs = NewFilesystem(auth, filepath.Join(testDBLoc, "test"))
	server, _ := fuse.NewServer(
		fs,
		mountLoc,
		&fuse.MountOptions{
			Name:          "onedriver",
			FsName:        "onedriver",
			DisableXAttrs: true,
			MaxBackground: 1024,
		},
	)

	// setup sigint handler for graceful unmount on interrupt/terminate
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGABRT)
	go UnmountHandler(sigChan, server)

	// mount fs in background thread
	go server.Serve()

	// cleanup from last run
	log.Info().Msg("Setup test environment ---------------------------------")
	if err := os.RemoveAll(TestDir); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	os.Mkdir(TestDir, 0755)
	os.Mkdir(DeltaDir, 0755)

	// create paging test files before the delta thread is created
	if !singleTest {
		os.Mkdir(filepath.Join(TestDir, "paging"), 0755)
		if live {
			createPagingTestFiles()
		} else {
			for i := 0; i < 250; i++ {
				mock.PutFile(fmt.Sprintf("/onedriver_tests/paging/%d.txt", i), []byte("test\n"))
			}
		}
	}
	go fs.DeltaLoop(5 * time.Second)

	// not created by default on onedrive for business
	os.Mkdir(mountLoc+"/Documents", 0755)

	// we do not cd into the mounted directory or it will hang indefinitely on
	// unmount with "device or resource busy"
	log.Info().Msg("Test session start ---------------------------------")

	// run tests
	code := m.Run()

	log.Info().Msg("Test session end -----------------------------------")
	fmt.Printf("Waiting 5 seconds for any remaining uploads to complete")
	for i := 0; i < 5; i++ {
		time.Sleep(time.Second)
		fmt.Printf(".")
	}
	fmt.Printf("\n")

	// unmount
	if server.Unmount() != nil {
		log.Error().Msg("Failed to unmount test fuse server, attempting lazy unmount")
		runErr("fusermount3", "-zu", "mount")
	}
	fmt.Println("Successfully unmounted fuse server!")
	stopRunner()
	if !live {
		mock.Close()
		if code == 0 {
			os.Chdir("/")
			os.RemoveAll(workDir)
		} else {
			fmt.Println("Test logs and caches kept in", workDir)
		}
	}
	os.Exit(code)
}

// Apparently 200 reqests is the default paging limit.
// Upload at least this many for a later test before the delta thread is created.
func createPagingTestFiles() {
	fmt.Println("Setting up paging test files.")
	var group sync.WaitGroup
	var errCounter int64
	for i := 0; i < 250; i++ {
		group.Add(1)
		go func(n int, wg *sync.WaitGroup) {
			_, err := graph.Put(
				graph.ResourcePath(fmt.Sprintf("/onedriver_tests/paging/%d.txt", n))+":/content",
				auth,
				strings.NewReader("test\n"),
			)
			if err != nil {
				log.Error().Err(err).Msg("Paging upload fail.")
				atomic.AddInt64(&errCounter, 1)
			}
			wg.Done()
		}(i, &group)
	}
	group.Wait()
	log.Info().Msgf("%d failed paging uploads.\n", errCounter)
	fmt.Println("Finished with paging test setup.")
}
