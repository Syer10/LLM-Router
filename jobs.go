// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Job is a GPU-intensive command run on a cron schedule, but only while the
// router's GPU is free (no koboldcpp model loaded). It rides on the idle-unload
// mechanism: traffic stops, the model unloads after idle_unload, VRAM frees,
// and the job becomes eligible.
type Job struct {
	Name     string        `yaml:"name"`
	Schedule string        `yaml:"schedule"` // cron expression (5 fields)
	Command  string        `yaml:"command"`
	WorkDir  string        `yaml:"workdir"`
	Env      []string      `yaml:"env"`
	// Timeout caps a single run; 0 = no limit.
	Timeout time.Duration `yaml:"timeout"`
	// OnBusy is what to do when the GPU is busy at the scheduled time:
	// "defer" (default) waits for the GPU to free, "skip" skips the run.
	OnBusy string `yaml:"on_busy"`
	// MaxWait bounds a deferred run; 0 = wait until the GPU is free.
	MaxWait time.Duration `yaml:"max_wait"`
}

// jobState is the runtime state for one configured job.
type jobState struct {
	job  Job
	spec cron.Schedule

	mu          sync.Mutex
	next        time.Time
	scheduledAt time.Time
	running     *exec.Cmd
	cancel      context.CancelFunc
	logs        *logBuffer
	lastRun     time.Time
	lastExit    int // -1 = never run, -2 = skipped
	lastErr     string
}

// jobRunner schedules and supervises the configured jobs.
type jobRunner struct {
	r    *Router
	jobs []*jobState
}

func newJobRunner(r *Router) *jobRunner {
	jr := &jobRunner{r: r}
	now := time.Now()
	for i := range r.cfg.Jobs {
		job := r.cfg.Jobs[i]
		spec, err := cron.ParseStandard(job.Schedule)
		if err != nil {
			// LoadConfig already rejects bad schedules; this only guards
			// direct NewRouter calls (tests).
			log.Printf("job %q: invalid schedule %q: %v (skipped)", job.Name, job.Schedule, err)
			continue
		}
		if job.OnBusy == "" {
			job.OnBusy = "defer"
		}
		jr.jobs = append(jr.jobs, &jobState{
			job:      job,
			spec:     spec,
			next:     spec.Next(now),
			logs:     newLogBuffer(512 * 1024),
			lastExit: -1,
		})
	}
	return jr
}

// start launches the scheduler loop. It is a no-op when there are no jobs.
func (jr *jobRunner) start(ctx context.Context) {
	if len(jr.jobs) == 0 {
		return
	}
	go jr.loop(ctx)
}

func (jr *jobRunner) loop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			jr.tick(now)
		}
	}
}

// tick decides, for each due job, whether to run it, defer it, or skip it.
func (jr *jobRunner) tick(now time.Time) {
	idle := jr.r.GPUIdle()
	for _, js := range jr.jobs {
		if js.tickOnce(now, idle) == actRun {
			jr.startJob(js)
		}
	}
}

type action int

const (
	actNone action = iota
	actRun
	actDefer
	actSkip
	actRunning
)

// tickOnce evaluates one job at `now`. It returns actRun when the caller
// should start the job; defer, skip, and already-running outcomes are handled
// here.
func (js *jobState) tickOnce(now time.Time, idle bool) action {
	js.mu.Lock()
	defer js.mu.Unlock()
	if now.Before(js.next) {
		return actNone
	}
	if js.scheduledAt.IsZero() {
		js.scheduledAt = js.next
	}
	if !idle {
		switch js.job.OnBusy {
		case "skip":
			return js.markSkippedLocked(now, "GPU busy at scheduled time")
		case "defer":
			if js.job.MaxWait > 0 && now.Sub(js.scheduledAt) > js.job.MaxWait {
				return js.markSkippedLocked(now, "GPU busy past max_wait")
			}
		}
		return actDefer
	}
	if js.running != nil {
		// A deferred run is still going; do not start a second instance.
		js.advanceLocked(now)
		return actRunning
	}
	js.advanceLocked(now)
	return actRun
}

func (js *jobState) advanceLocked(now time.Time) {
	js.next = js.spec.Next(now)
	js.scheduledAt = time.Time{}
}

func (js *jobState) markSkippedLocked(now time.Time, reason string) action {
	log.Printf("[job %s] %s, skipping", js.job.Name, reason)
	js.lastRun = now
	js.lastExit = -2
	js.lastErr = reason
	js.advanceLocked(now)
	return actSkip
}

// startJob spawns the job's command and supervises it to completion.
func (jr *jobRunner) startJob(js *jobState) {
	args := splitCommand(js.job.Command)
	if len(args) == 0 {
		log.Printf("[job %s] empty command, not started", js.job.Name)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	if js.job.Timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), js.job.Timeout)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	// Kill the whole process tree (not just the parent) on timeout/cancel.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killProcessTree(cmd.Process.Pid)
	}
	cmd.WaitDelay = 15 * time.Second
	cmd.Dir = js.job.WorkDir
	cmd.Env = append(os.Environ(), js.job.Env...)
	cmd.SysProcAttr = procGroupAttr()
	buf := newLogBuffer(512 * 1024)
	cmd.Stdout = buf
	cmd.Stderr = buf

	js.mu.Lock()
	js.running = cmd
	js.cancel = cancel
	js.logs = buf
	js.mu.Unlock()

	log.Printf("[job %s] starting: %s", js.job.Name, js.job.Command)
	if err := cmd.Start(); err != nil {
		log.Printf("[job %s] start failed: %v", js.job.Name, err)
		js.finish(-1, "start: "+err.Error())
		return
	}
	log.Printf("[job %s] started (pid %d)", js.job.Name, cmd.Process.Pid)
	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		js.finish(code, "")
	}()
}

// finish records the outcome of a run. It is called exactly once per run, from
// the cmd.Wait goroutine.
func (js *jobState) finish(code int, errMsg string) {
	js.mu.Lock()
	js.running = nil
	js.cancel = nil
	js.lastRun = time.Now()
	js.lastExit = code
	if errMsg != "" {
		js.lastErr = errMsg
	}
	js.mu.Unlock()
	switch {
	case code == 0:
		log.Printf("[job %s] finished successfully", js.job.Name)
	case errMsg != "":
		log.Printf("[job %s] finished with exit %d (%s)", js.job.Name, code, errMsg)
	default:
		log.Printf("[job %s] finished with exit %d", js.job.Name, code)
	}
}

// stop kills a running job (preemption, shutdown, or timeout). Safe to call
// when nothing is running.
func (js *jobState) stop() {
	js.mu.Lock()
	cancel := js.cancel
	running := js.running != nil
	js.mu.Unlock()
	if running && cancel != nil {
		cancel()
	}
}

func (js *jobState) isRunning() bool {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.running != nil
}

// preempt kills every running job so the GPU is free for a model load. It is
// called from the proxy when a request is routed to a koboldcpp backend.
func (jr *jobRunner) preempt() {
	for _, js := range jr.jobs {
		if js.isRunning() {
			log.Printf("[job %s] preempted by LLM traffic", js.job.Name)
			js.stop()
		}
	}
}

// stopAll kills every running job (shutdown path).
func (jr *jobRunner) stopAll() {
	for _, js := range jr.jobs {
		js.stop()
	}
}

func (jr *jobRunner) byName(name string) (*jobState, bool) {
	for _, js := range jr.jobs {
		if js.job.Name == name {
			return js, true
		}
	}
	return nil, false
}

// runNow starts a job immediately, bypassing the idle check (the operator
// explicitly wants it now).
func (jr *jobRunner) runNow(name string) error {
	js, ok := jr.byName(name)
	if !ok {
		return fmt.Errorf("unknown job %q", name)
	}
	if js.isRunning() {
		return fmt.Errorf("job %q is already running", name)
	}
	if !jr.r.GPUIdle() {
		log.Printf("[job %s] manual run while the GPU is busy; may contend for VRAM", name)
	}
	jr.startJob(js)
	return nil
}

func (js *jobState) Logs(lines int) string {
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.logs == nil {
		return ""
	}
	return js.logs.Tail(lines)
}

// GPUIdle reports whether no koboldcpp backend currently has a model loaded,
// i.e. the local GPU's VRAM is free for a background job. OpenAI backends do
// not use the local GPU, so they are ignored.
func (r *Router) GPUIdle() bool {
	for _, name := range r.order {
		if r.cfgs[name].Type != "koboldcpp" {
			continue
		}
		if s := r.backends[name].State(); s == StateReady || s == StateSwapping {
			return false
		}
	}
	return true
}

// --- admin API ---

type jobInfo struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
	OnBusy   string `json:"on_busy"`
	MaxWait  string `json:"max_wait,omitempty"`
	NextRun  string `json:"next_run"`
	LastRun  string `json:"last_run,omitempty"`
	LastExit int    `json:"last_exit"`
	LastErr  string `json:"last_error,omitempty"`
	Running  bool   `json:"running"`
}

func (js *jobState) info() jobInfo {
	js.mu.Lock()
	defer js.mu.Unlock()
	ji := jobInfo{
		Name:     js.job.Name,
		Schedule: js.job.Schedule,
		Command:  js.job.Command,
		OnBusy:   js.job.OnBusy,
		NextRun:  js.next.Format(time.RFC3339),
		LastExit: js.lastExit,
		Running:  js.running != nil,
	}
	if js.job.MaxWait > 0 {
		ji.MaxWait = js.job.MaxWait.String()
	}
	if !js.lastRun.IsZero() {
		ji.LastRun = js.lastRun.Format(time.RFC3339)
		ji.LastErr = js.lastErr
	}
	return ji
}

func (jr *jobRunner) handleList(w http.ResponseWriter) {
	out := []jobInfo{}
	for _, js := range jr.jobs {
		out = append(out, js.info())
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (jr *jobRunner) handleOne(w http.ResponseWriter, name string) {
	js, ok := jr.byName(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown job " + name})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": js.info()})
}

func (jr *jobRunner) handleLogs(w http.ResponseWriter, req *http.Request, name string) {
	js, ok := jr.byName(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown job " + name})
		return
	}
	lines := 100
	if v := req.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lines = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": name, "logs": js.Logs(lines)})
}
