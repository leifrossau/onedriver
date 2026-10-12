package fs

// Test helper process for running external commands.
//
// A second hazard this works around: the first time a process opens a regular
// file on a FUSE mount, Go registers it with epoll, which makes the kernel
// send a FUSE_POLL request (the kernel stops sending them once it got ENOSYS).
// If that happens in the test process, the goroutine sits in a raw epoll_ctl
// syscall that the scheduler cannot preempt, so a garbage collection
// stop-the-world that starts meanwhile waits for it while the FUSE server (in
// the same process) cannot run to answer: deadlock. warmMount makes the
// helper process do that first open for each mount.
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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
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
	fdSock = os.NewFile(3, "fdsock")
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
		case "warm":
			// create + remove a file: epoll registration sends FUSE_POLL
			name := filepath.Join(req.Dir, ".onedriver-test-warmup")
			f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0644)
			if err != nil {
				resp.ExitCode, resp.Err = -1, err.Error()
				break
			}
			f.Close()
			os.Remove(name)
		case "mount":
			if err := runnerMount(req); err != nil {
				resp.ExitCode, resp.Err = -1, err.Error()
			}
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
	fds     map[int64]chan int
}

var runner *runnerClient

// startRunner launches the helper process. It must be called before any
// filesystem is mounted by this process.
func startRunner() error {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), runnerEnv+"=1")
	cmd.Stderr = os.Stderr
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	mine := os.NewFile(uintptr(pair[0]), "fdsock-local")
	theirs := os.NewFile(uintptr(pair[1]), "fdsock-runner")
	cmd.ExtraFiles = []*os.File{theirs}
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
	theirs.Close()
	r := &runnerClient{cmd: cmd, stdin: in, enc: json.NewEncoder(in),
		pending: map[int64]chan runnerResponse{}, fds: map[int64]chan int{}}
	go r.receiveFds(mine)
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

func (r *runnerClient) nextRequestID() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	return r.nextID
}

func (r *runnerClient) do(req runnerRequest) runnerResponse {
	return r.doID(r.nextRequestID(), req)
}

func (r *runnerClient) doID(id int64, req runnerRequest) runnerResponse {
	ch := make(chan runnerResponse, 1)
	r.mu.Lock()
	req.ID = id
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

// fdSock is the runner's end of the socket used to hand /dev/fuse descriptors
// to the test process.
var (
	fdSock *os.File
	fdMu   sync.Mutex
)

// runnerMount mounts a FUSE filesystem the way go-fuse does (fusermount3 with
// _FUSE_COMMFD) and passes the resulting /dev/fuse descriptor to the test
// process over fdSock, tagged with the request ID. This way the test process
// never has to fork while it serves other mounts.
func runnerMount(req runnerRequest) error {
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	local := os.NewFile(uintptr(pair[0]), "local")
	remote := os.NewFile(uintptr(pair[1]), "remote")
	defer local.Close()
	defer remote.Close()

	cmd := exec.Command("fusermount3", append([]string{req.Name}, req.Args...)...)
	cmd.Env = []string{"_FUSE_COMMFD=3"}
	cmd.ExtraFiles = []*os.File{remote}
	var se bytes.Buffer
	cmd.Stderr = &se
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fusermount3: %v: %s", err, se.String())
	}
	buf := make([]byte, 4)
	oob := make([]byte, syscall.CmsgSpace(4))
	_, oobn, _, _, err := syscall.Recvmsg(pair[0], buf, oob, 0)
	if err != nil {
		return err
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) == 0 {
		return fmt.Errorf("no control message from fusermount3: %v", err)
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return fmt.Errorf("no fd from fusermount3: %v", err)
	}
	defer syscall.Close(fds[0])

	id := make([]byte, 8)
	for i := 0; i < 8; i++ {
		id[i] = byte(req.ID >> (8 * i))
	}
	fdMu.Lock()
	defer fdMu.Unlock()
	return syscall.Sendmsg(int(fdSock.Fd()), id, syscall.UnixRights(fds[0]), nil, 0)
}

// receiveFds reads descriptors sent by runnerMount.
func (r *runnerClient) receiveFds(sock *os.File) {
	for {
		buf := make([]byte, 8)
		oob := make([]byte, syscall.CmsgSpace(4))
		n, oobn, _, _, err := syscall.Recvmsg(int(sock.Fd()), buf, oob, 0)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return
		}
		if n != 8 {
			continue
		}
		var id int64
		for i := 0; i < 8; i++ {
			id |= int64(buf[i]) << (8 * i)
		}
		fd := -1
		if msgs, err := syscall.ParseSocketControlMessage(oob[:oobn]); err == nil && len(msgs) > 0 {
			if fds, err := syscall.ParseUnixRights(&msgs[0]); err == nil && len(fds) == 1 {
				fd = fds[0]
			}
		}
		r.fdChan(id) <- fd
	}
}

func (r *runnerClient) fdChan(id int64) chan int {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := r.fds[id]
	if ch == nil {
		ch = make(chan int, 1)
		r.fds[id] = ch
	}
	return ch
}

// mountFuse mounts fsys at mnt like fuse.NewServer does, except that
// fusermount3 is run by the helper process: forking in this process while
// other mounts served by it have open files can deadlock (see top of file).
// The server is already serving when it returns. Unmount with unmountFuse.
func mountFuse(fsys fuse.RawFileSystem, mnt string, opts *fuse.MountOptions) (*fuse.Server, error) {
	if runner == nil {
		return nil, errors.New("test runner not started")
	}
	abs, err := filepath.Abs(mnt)
	if err != nil {
		return nil, err
	}
	o := *opts
	if o.MaxWrite <= 0 {
		o.MaxWrite = 128 * 1024
	}
	var optStrs []string
	if o.FsName != "" {
		optStrs = append(optStrs, "fsname="+o.FsName)
	}
	if o.Name != "" {
		optStrs = append(optStrs, "subtype="+o.Name)
	}
	optStrs = append(optStrs, fmt.Sprintf("max_read=%d", o.MaxWrite))

	id := runner.nextRequestID()
	ch := runner.fdChan(id)
	resp := runner.doID(id, runnerRequest{Op: "mount", Name: abs, Args: []string{"-o", strings.Join(optStrs, ",")}})
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	var fd int
	select {
	case fd = <-ch:
	case <-time.After(30 * time.Second):
		return nil, errors.New("timeout waiting for /dev/fuse descriptor from runner")
	}
	runner.mu.Lock()
	delete(runner.fds, id)
	runner.mu.Unlock()
	if fd < 0 {
		return nil, errors.New("runner sent no descriptor")
	}
	srv, err := fuse.NewServer(fsys, fmt.Sprintf("/dev/fd/%d", fd), &o)
	if err != nil {
		syscall.Close(fd)
		runErr("fusermount3", "-uz", abs)
		return nil, err
	}
	go srv.Serve()
	if err := srv.WaitMount(); err != nil {
		return nil, err
	}
	// see the top of this file
	if resp := runner.do(runnerRequest{Op: "warm", Dir: abs}); resp.Err != "" {
		return nil, errors.New("warming up mount: " + resp.Err)
	}
	return srv, nil
}

// unmountFuse unmounts a filesystem mounted with mountFuse, falling back to a
// lazy unmount. It waits for the server loop to finish.
func unmountFuse(srv *fuse.Server, mnt string) error {
	abs, err := filepath.Abs(mnt)
	if err != nil {
		abs = mnt
	}
	delay := time.Duration(0)
	for try := 0; try < 5; try++ {
		if err = runErr("fusermount3", "-u", abs); err == nil {
			break
		}
		delay = 2*delay + 5*time.Millisecond
		time.Sleep(delay)
	}
	if err != nil {
		runErr("fusermount3", "-uz", abs)
	}
	srv.Wait()
	return err
}
