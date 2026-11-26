package backend

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultKernelIdleTimeout = 3 * time.Minute
	maxKernelRequestBytes    = 128 * 1024
	kernelStderrRingBytes    = 4096
)

type kernelCommandFactory func(jarPath string, asShell bool) *exec.Cmd

// stderrRing keeps the trailing kernelStderrRingBytes of subprocess stderr without unbounded growth.
type stderrRing struct {
	mu  sync.Mutex
	buf []byte
}

func (r *stderrRing) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= kernelStderrRingBytes {
		r.buf = append(r.buf[:0], p[len(p)-kernelStderrRingBytes:]...)
		return len(p), nil
	}
	r.buf = append(r.buf, p...)
	if len(r.buf) > kernelStderrRingBytes {
		drop := len(r.buf) - kernelStderrRingBytes
		copy(r.buf, r.buf[drop:])
		r.buf = r.buf[:kernelStderrRingBytes]
	}
	return len(p), nil
}

func (r *stderrRing) String() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(string(r.buf))
}

type kernelRequest struct {
	Cmd      string `json:"cmd"`
	Text     string `json:"text,omitempty"`
	MaxDepth int    `json:"max_depth,omitempty"`
	MaxNodes int    `json:"max_nodes,omitempty"`
}

type kernelResponse struct {
	OK        bool        `json:"ok"`
	Pong      bool        `json:"pong,omitempty"`
	Error     string      `json:"error,omitempty"`
	Text      string      `json:"text,omitempty"`
	Package   string      `json:"pkg,omitempty"`
	Width     int         `json:"w,omitempty"`
	Height    int         `json:"h,omitempty"`
	Rotation  int         `json:"rot,omitempty"`
	RawCount  int         `json:"raw,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
	Nodes     []RawUINode `json:"nodes,omitempty"`
}

type kernelProc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *stderrRing
	lines   chan string
	readErr chan error
	stopCh  chan struct{}
	stopOnce sync.Once
	done    chan struct{}
}

func (p *kernelProc) closeStop() {
	if p == nil || p.stopCh == nil {
		return
	}
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
}

// MicroKernel lazily starts and manages a persistent `app_process64 AgentKernel` child process
// over stdin/stdout JSONL pipes. It shuts down automatically after IdleTimeout of inactivity
// or immediately when Stop is called (e.g. game-guard trigger).
type MicroKernel struct {
	ensureJar   func() (string, error)
	cmdFactory  kernelCommandFactory
	idleTimeout time.Duration

	callMu sync.Mutex // serializes stdin/stdout round-trips

	stateMu   sync.Mutex
	proc      *kernelProc
	gen       uint64
	stopSeq   uint64
	idleTimer *time.Timer
}

func NewMicroKernel(ensureJar func() (string, error), idleTimeout time.Duration) *MicroKernel {
	if idleTimeout <= 0 {
		idleTimeout = defaultKernelIdleTimeout
	}
	mk := &MicroKernel{
		ensureJar:   ensureJar,
		idleTimeout: idleTimeout,
	}
	mk.cmdFactory = func(jarPath string, asShell bool) *exec.Cmd {
		cmd := exec.Command("/system/bin/app_process64", "/system/bin", "AgentKernel")
		cmd.Env = append(os.Environ(),
			"CLASSPATH="+jarPath,
			"ANDROID_DATA=/data/local/tmp",
		)
		if asShell && os.Geteuid() == 0 {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Credential: &syscall.Credential{Uid: 2000, Gid: 2000},
			}
		}
		return cmd
	}
	return mk
}

// Running reports whether the micro-kernel child process is currently alive.
func (m *MicroKernel) Running() bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.proc == nil {
		return false
	}
	select {
	case <-m.proc.done:
		return false
	default:
		return true
	}
}

// Stop kills the micro-kernel child process immediately (e.g. when guarded game opens or on shutdown).
func (m *MicroKernel) Stop(_ string) {
	m.stateMu.Lock()
	m.stopSeq++
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	p := m.proc
	m.proc = nil
	m.gen++
	m.stateMu.Unlock()

	terminateKernelProc(p)
}

func terminateKernelProc(p *kernelProc) {
	if p == nil {
		return
	}
	p.closeStop()
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	if p.done != nil {
		select {
		case <-p.done:
		case <-time.After(1 * time.Second):
		}
	}
}

func (m *MicroKernel) startLocked() (*kernelProc, error) {
	m.stateMu.Lock()
	seqBefore := m.stopSeq
	m.stateMu.Unlock()

	var jarPath string
	if m.ensureJar != nil {
		p, err := m.ensureJar()
		if err != nil {
			return nil, fmt.Errorf("prepare AgentKernel jar: %w", err)
		}
		jarPath = p
	}

	spawnOnce := func(asShell bool) (*kernelProc, error) {
		cmd := m.cmdFactory(jarPath, asShell)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			_ = stdin.Close()
			return nil, err
		}
		errRing := &stderrRing{}
		cmd.Stderr = errRing
		if err := cmd.Start(); err != nil {
			_ = stdin.Close()
			_ = stdout.Close()
			return nil, err
		}
		kp := &kernelProc{
			cmd:     cmd,
			stdin:   stdin,
			stderr:  errRing,
			lines:   make(chan string, 8),
			readErr: make(chan error, 1),
			stopCh:  make(chan struct{}),
			done:    make(chan struct{}),
		}
		go func(r io.ReadCloser, lp *kernelProc) {
			defer close(lp.done)
			defer close(lp.readErr)
			defer close(lp.lines)
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
			for sc.Scan() {
				line := sc.Text()
				select {
				case lp.lines <- line:
				case <-lp.stopCh:
					goto exitReader
				default:
					// Avoid deadlocking the reader goroutine if spurious stdout fills the buffer:
					// drop the oldest stale buffered line and enqueue the latest frame.
					select {
					case <-lp.lines:
					default:
					}
					select {
					case lp.lines <- line:
					case <-lp.stopCh:
						goto exitReader
					}
				}
			}
		exitReader:
			if err := sc.Err(); err != nil {
				lp.readErr <- err
			} else {
				lp.readErr <- io.EOF
			}
			_ = r.Close()
			_ = lp.cmd.Wait()
		}(stdout, kp)
		return kp, nil
	}

	kp, err := spawnOnce(true)
	if err != nil && os.Geteuid() == 0 {
		kp, err = spawnOnce(false)
	}
	if err != nil {
		return nil, fmt.Errorf("start AgentKernel: %w", err)
	}

	m.stateMu.Lock()
	if m.stopSeq != seqBefore {
		m.stateMu.Unlock()
		terminateKernelProc(kp)
		return nil, errors.New("microkernel stopped during startup")
	}
	old := m.proc
	m.proc = kp
	m.gen++
	gen := m.gen
	m.resetIdleLocked(gen)
	m.stateMu.Unlock()

	if old != nil {
		terminateKernelProc(old)
	}
	return kp, nil
}

func (m *MicroKernel) resetIdleLocked(gen uint64) {
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.idleTimer = time.AfterFunc(m.idleTimeout, func() {
		m.stateMu.Lock()
		if m.gen != gen || m.proc == nil {
			m.stateMu.Unlock()
			return
		}
		p := m.proc
		m.proc = nil
		m.gen++
		m.idleTimer = nil
		m.stateMu.Unlock()
		terminateKernelProc(p)
	})
}

func (m *MicroKernel) getOrStart() (*kernelProc, uint64, error) {
	m.stateMu.Lock()
	if m.proc != nil {
		select {
		case <-m.proc.done:
			// Previous child exited asynchronously; respawn cleanly below.
			m.proc = nil
		default:
			p := m.proc
			gen := m.gen
			m.resetIdleLocked(gen)
			m.stateMu.Unlock()
			return p, gen, nil
		}
	}
	m.stateMu.Unlock()

	p, err := m.startLocked()
	if err != nil {
		return nil, 0, err
	}
	m.stateMu.Lock()
	gen := m.gen
	m.stateMu.Unlock()
	return p, gen, nil
}

func (m *MicroKernel) invalidateProc(p *kernelProc) {
	m.stateMu.Lock()
	if m.proc == p {
		if m.idleTimer != nil {
			m.idleTimer.Stop()
			m.idleTimer = nil
		}
		m.proc = nil
		m.gen++
	}
	m.stateMu.Unlock()
	terminateKernelProc(p)
}

func (m *MicroKernel) Call(ctx context.Context, req kernelRequest) (*kernelResponse, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(payload)+1 > maxKernelRequestBytes {
		return nil, fmt.Errorf("AgentKernel request exceeds %d bytes limit (%d bytes)", maxKernelRequestBytes, len(payload)+1)
	}
	payload = append(payload, '\n')

	m.callMu.Lock()
	defer m.callMu.Unlock()

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		kp, gen, err := m.getOrStart()
		if err != nil {
			return nil, err
		}
		resp, err := m.roundTrip(ctx, kp, payload)
		if err == nil {
			m.stateMu.Lock()
			if m.proc == kp && m.gen == gen {
				m.resetIdleLocked(gen)
			}
			m.stateMu.Unlock()
			return resp, nil
		}
		lastErr = err
		m.invalidateProc(kp)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (m *MicroKernel) roundTrip(ctx context.Context, kp *kernelProc, payload []byte) (*kernelResponse, error) {

	writeDone := make(chan error, 1)
	go func() {
		_, werr := kp.stdin.Write(payload)
		writeDone <- werr
	}()

	select {
	case <-ctx.Done():
		_ = kp.stdin.Close()
		return nil, ctx.Err()
	case werr := <-writeDone:
		if werr != nil {
			if st := kp.stderr.String(); st != "" {
				return nil, fmt.Errorf("write AgentKernel stdin: %w (stderr: %s)", werr, st)
			}
			return nil, fmt.Errorf("write AgentKernel stdin: %w", werr)
		}
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case line, ok := <-kp.lines:
		if !ok {
			rerr := <-kp.readErr
			if rerr == nil {
				rerr = io.EOF
			}
			if st := kp.stderr.String(); st != "" {
				return nil, fmt.Errorf("AgentKernel exited: %w (stderr: %s)", rerr, st)
			}
			return nil, fmt.Errorf("AgentKernel exited: %w", rerr)
		}
		var resp kernelResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			if st := kp.stderr.String(); st != "" {
				return nil, fmt.Errorf("decode AgentKernel response: %w (stderr: %s)", err, st)
			}
			return nil, fmt.Errorf("decode AgentKernel response: %w", err)
		}
		if !resp.OK {
			msg := resp.Error
			if msg == "" {
				msg = "AgentKernel returned ok=false"
			}
			return &resp, errors.New(msg)
		}
		return &resp, nil
	}
}

func fileMatchesBytes(path string, want []byte) bool {
	existing, err := os.ReadFile(path)
	if err != nil || len(existing) != len(want) {
		return false
	}
	return sha256.Sum256(existing) == sha256.Sum256(want)
}
