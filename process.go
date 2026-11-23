package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// logBuffer is a bounded ring buffer used to capture backend process output.
type logBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newLogBuffer(max int) *logBuffer { return &logBuffer{max: max} }

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) > b.max {
		p = p[len(p)-b.max:]
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *logBuffer) Tail(lines int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) == 0 {
		return ""
	}
	all := strings.Split(string(b.buf), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// ProcessManager spawns and supervises one backend process.
type ProcessManager struct {
	name         string
	command      string
	workdir      string
	env          []string
	healthURL    string
	startTimeout time.Duration

	mu        sync.Mutex
	cmd       *exec.Cmd
	dead      chan struct{} // closed when the process exits
	logs      *logBuffer
	startedAt time.Time

	stateMu sync.RWMutex
	state   State
}

func NewProcessManager(name, command, workdir string, env []string, healthURL string, startTimeout time.Duration) *ProcessManager {
	return &ProcessManager{
		name:         name,
		command:      command,
		workdir:      workdir,
		env:          env,
		healthURL:    healthURL,
		startTimeout: startTimeout,
		logs:         newLogBuffer(512 * 1024),
		state:        StateStopped,
	}
}

func (p *ProcessManager) setState(s State) {
	p.stateMu.Lock()
	p.state = s
	p.stateMu.Unlock()
}

func (p *ProcessManager) State() State {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.state
}

func (p *ProcessManager) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *ProcessManager) Alive() bool {
	p.mu.Lock()
	cmd, dead := p.cmd, p.dead
	p.mu.Unlock()
	if cmd == nil {
		return false
	}
	select {
	case <-dead:
		return false
	default:
		return true
	}
}

func (p *ProcessManager) StartedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startedAt
}

func (p *ProcessManager) Logs(lines int) string {
	return p.logs.Tail(lines)
}

// Start spawns the process and waits until its health endpoint answers.
// It is a no-op when the process is already running.
func (p *ProcessManager) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.cmd != nil {
		select {
		case <-p.dead:
			// Previous process died; fall through and restart.
		default:
			p.mu.Unlock()
			return nil // already running
		}
	}
	args := splitCommand(p.command)
	if len(args) == 0 {
		p.mu.Unlock()
		return fmt.Errorf("empty command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = p.workdir
	cmd.Env = append(os.Environ(), p.env...)
	cmd.SysProcAttr = procGroupAttr()
	cmd.Stdout = p.logs
	cmd.Stderr = p.logs

	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		return fmt.Errorf("starting process: %w", err)
	}
	p.cmd = cmd
	p.dead = make(chan struct{})
	p.startedAt = time.Now()
	p.mu.Unlock()

	go func() {
		err := cmd.Wait()
		close(p.dead)
		log.Printf("[%s] process (pid %d) exited: %v", p.name, cmd.Process.Pid, err)
		p.setState(StateStopped)
	}()

	p.setState(StateStarting)
	if err := p.waitForHealth(ctx); err != nil {
		p.Stop()
		p.setState(StateUnhealthy)
		return err
	}
	p.setState(StateReady)
	return nil
}

// Stop kills the process tree. Safe to call when nothing is running.
func (p *ProcessManager) Stop() {
	p.mu.Lock()
	cmd := p.cmd
	p.cmd = nil
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := killProcessTree(cmd.Process.Pid); err != nil {
		log.Printf("[%s] killing process tree: %v", p.name, err)
	}
}

// waitForHealth polls the health URL until any HTTP response arrives
// (the server may answer with an error while a model is still loading;
// a response at all means the process is up).
func (p *ProcessManager) waitForHealth(ctx context.Context) error {
	deadline := time.Now().Add(p.startTimeout)
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		if !p.Alive() {
			return fmt.Errorf("process exited during startup")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.healthURL, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health check timed out after %s at %s", p.startTimeout, p.healthURL)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// splitCommand splits a command line on spaces, honoring single and double
// quotes. Good enough for backend launch commands; no shell features.
func splitCommand(s string) []string {
	var args []string
	var cur strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ' ' || r == '\t':
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}
