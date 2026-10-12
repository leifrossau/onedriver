package fs

// Test helper process for running external commands.
//
// The fs tests mount real FUSE filesystems served by the test process itself.
// If the test process forks (exec.Command) while any goroutine holds an open
// file on such a mount, the forked child (which shares the address space until
// it execs, thanks to CLONE_VFORK) can close/inherit that descriptor, which
// sends FLUSH/RELEASE requests to the FUSE server in the very process that is
// blocked waiting for the child: a deadlock. To avoid this, all external
// commands are run by a helper process (this test binary re-executed with
// ONEDRIVER_TEST_RUNNER=1) that is started before anything is mounted and thus
// never holds a descriptor on a mount.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

const runnerEnv = "ONEDRIVER_TEST_RUNNER"

type runnerRequest struct {
	ID    int64
	Op    string // "run", "daemon" or "kill"
	Dir   string
	Name  string
	Args  []string
	Stdin []byte
	Pid   int
}

type runnerResponse struct {
	ID       int64
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Err      string
	Pid      int
}

// runnerMain is the helper process: it serves requests from stdin until EOF.
func runnerMain() {
	var wmu sync.Mutex
	out := json.NewEncoder(os.Stdout)
	send := func(r runnerResponse) {
		wmu.Lock()
		defer wmu.Unlock()
		out.Encode(r)
	}
	var dmu sync.Mutex
	daemons := map[int]*exec.Cmd{}

	handle := func(req runnerRequest) {
		resp := runnerResponse{ID: req.ID}
		switch req.Op {
		case "run":
			cmd := exec.Command(req.Name, req.Args...)
			cmd.Dir = req.Dir
			if req.Stdin != nil {
				cmd.Stdin = bytes.NewReader(req.Stdin)
			}
			var so, se bytes.Buffer
			cmd.Stdout, cmd.Stderr = &so, &se
			err := cmd.Run()
			resp.Stdout, resp.Stderr = so.Bytes(), se.Bytes()
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				resp.ExitCode = ee.ExitCode()
			} else if err != nil {
				resp.ExitCode = -1
				resp.Err = err.Error()
			}
		case "daemon":
			cmd := exec.Command(req.Name, req.Args...)
			cmd.Dir = req.Dir
			pipe, err := cmd.StdoutPipe()
			if err == nil {
				err = cmd.Start()
			}
			if err != nil {
				resp.ExitCode, resp.Err = -1, err.Error()
				break
			}
			line, err := bufio.NewReader(pipe).ReadString('\n')
			if err != nil {
				cmd.Process.Kill()
				cmd.Wait()
				resp.ExitCode, resp.Err = -1, err.Error()
				break
			}
			resp.Stdout = []byte(line)
			resp.Pid = cmd.Process.Pid
			dmu.Lock()
			daemons[resp.Pid] = cmd
			dmu.Unlock()
			go io.Copy(io.Discard, pipe)
		case "kill":
			dmu.Lock()
			cmd := daemons[req.Pid]
			delete(daemons, req.Pid)
			dmu.Unlock()
			if cmd != nil {
				cmd.Process.Kill()
			}
		}
		send(resp)
	}

	dec := json.NewDecoder(bufio.NewReader(os.Stdin))
	var wg sync.WaitGroup
	for {
		var req runnerRequest
		if err := dec.Decode(&req); err != nil {
			break
		}
		wg.Add(1)
		go func() { defer wg.Done(); handle(req) }()
	}
	wg.Wait()
	dmu.Lock()
	for _, cmd := range daemons {
		cmd.Process.Kill()
	}
	dmu.Unlock()
}

type runnerClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	enc     *json.Encoder
	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan runnerResponse
}

var runner *runnerClient

// startRunner launches the helper process. It must be called before any
// filesystem is mounted by this process.
func startRunner() error {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), runnerEnv+"=1")
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	r := &runnerClient{cmd: cmd, stdin: in, enc: json.NewEncoder(in), pending: map[int64]chan runnerResponse{}}
	go func() {
		dec := json.NewDecoder(bufio.NewReader(outPipe))
		for {
			var resp runnerResponse
			if err := dec.Decode(&resp); err != nil {
				break
			}
			r.mu.Lock()
			ch := r.pending[resp.ID]
			delete(r.pending, resp.ID)
			r.mu.Unlock()
			if ch != nil {
				ch <- resp
			}
		}
		// runner gone: fail anything still waiting
		r.mu.Lock()
		for id, ch := range r.pending {
			ch <- runnerResponse{ID: id, ExitCode: -1, Err: "test runner exited"}
		}
		r.pending = map[int64]chan runnerResponse{}
		r.mu.Unlock()
	}()
	runner = r
	return nil
}

func stopRunner() {
	if runner == nil {
		return
	}
	runner.stdin.Close()
	runner.cmd.Wait()
	runner = nil
}

func (r *runnerClient) do(req runnerRequest) runnerResponse {
	ch := make(chan runnerResponse, 1)
	r.mu.Lock()
	r.nextID++
	req.ID = r.nextID
	r.pending[req.ID] = ch
	r.mu.Unlock()
	r.wmu.Lock()
	err := r.enc.Encode(req)
	r.wmu.Unlock()
	if err != nil {
		return runnerResponse{ExitCode: -1, Err: err.Error()}
	}
	return <-ch
}

// runCmd runs a command in the helper process (cwd = dir, or the helper's cwd,
// i.e. the test directory, when empty) and returns its output.
func runCmd(dir, name string, stdin []byte, args ...string) (stdout, stderr []byte, err error) {
	if runner == nil {
		return nil, nil, errors.New("test runner not started")
	}
	resp := runner.do(runnerRequest{Op: "run", Dir: dir, Name: name, Args: args, Stdin: stdin})
	switch {
	case resp.Err != "":
		err = errors.New(resp.Err)
	case resp.ExitCode != 0:
		err = fmt.Errorf("%s %s: exit status %d: %s", name, strings.Join(args, " "),
			resp.ExitCode, strings.TrimSpace(string(resp.Stderr)))
	}
	return resp.Stdout, resp.Stderr, err
}

// run executes a command and returns its stdout (like exec.Cmd.Output).
func run(name string, args ...string) ([]byte, error) {
	out, _, err := runCmd("", name, nil, args...)
	return out, err
}

// runCombined executes a command and returns stdout and stderr concatenated
// (stderr appended; ordering between the two is not preserved).
func runCombined(name string, args ...string) ([]byte, error) {
	out, errOut, err := runCmd("", name, nil, args...)
	return append(out, errOut...), err
}

// runErr runs a command and only returns its error.
func runErr(name string, args ...string) error {
	_, _, err := runCmd("", name, nil, args...)
	return err
}

// runCommand is run with a testing.T and an explicit working directory.
func runCommand(t *testing.T, dir, name string, args ...string) ([]byte, error) {
	t.Helper()
	out, _, err := runCmd(dir, name, nil, args...)
	return out, err
}

// startDaemon starts a long running command in the helper process, waits for
// its first line of stdout and returns it with a function that kills it.
func startDaemon(name string, args ...string) (firstLine string, stop func(), err error) {
	if runner == nil {
		return "", nil, errors.New("test runner not started")
	}
	resp := runner.do(runnerRequest{Op: "daemon", Name: name, Args: args})
	if resp.Err != "" {
		return "", nil, errors.New(resp.Err)
	}
	pid := resp.Pid
	return string(resp.Stdout), func() { runner.do(runnerRequest{Op: "kill", Pid: pid}) }, nil
}
