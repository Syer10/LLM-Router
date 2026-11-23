package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// DiscoverAll refreshes model lists and states for every backend.
func (r *Router) DiscoverAll(ctx context.Context) {
	for _, name := range r.order {
		if err := r.backends[name].Discover(ctx); err != nil {
			log.Printf("[%s] discovery: %v", name, err)
		}
	}
}

// StartOwned starts the processes of owned backends with auto_start.
func (r *Router) StartOwned(ctx context.Context) {
	var wg sync.WaitGroup
	for _, name := range r.order {
		bc := r.cfgs[name]
		if bc.Command == "" || !bc.AutoStartEnabled() {
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			log.Printf("[%s] starting process: %s", name, bc.Command)
			if err := r.backends[name].StartProcess(ctx); err != nil {
				log.Printf("[%s] failed to start: %v", name, err)
			} else {
				log.Printf("[%s] process ready (pid %d)", name, r.backends[name].PID())
			}
		}(name)
	}
	wg.Wait()
}

// StartLoops launches the periodic discovery and idle-management loops.
func (r *Router) StartLoops(ctx context.Context) {
	go func() {
		t := time.NewTicker(r.cfg.RefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.DiscoverAll(ctx)
			}
		}
	}()
	go r.idleLoop(ctx)
}

// idleLoop unloads models / stops owned processes after their idle timeout.
func (r *Router) idleLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, name := range r.order {
				b := r.backends[name]
				bc := r.cfgs[name]
				idle := now.Sub(b.LastActivity())
				owned := bc.Command != ""

				if bc.Type == "koboldcpp" {
					if bc.IdleUnload > 0 && idle >= bc.IdleUnload && b.State() == StateReady {
						log.Printf("[%s] idle for %s, unloading model", name, idle.Round(time.Second))
						go b.Release(context.Background())
					}
					if owned && bc.IdleStop > 0 && idle >= bc.IdleStop && b.PID() != 0 {
						log.Printf("[%s] idle for %s, stopping process", name, idle.Round(time.Second))
						b.StopProcess()
					}
				} else if owned && bc.IdleStop > 0 && idle >= bc.IdleStop && b.PID() != 0 {
					log.Printf("[%s] idle for %s, stopping process", name, idle.Round(time.Second))
					b.StopProcess()
				}
			}
		}
	}
}

// StopAll stops every owned process (shutdown path).
func (r *Router) StopAll() {
	for _, name := range r.order {
		if r.backends[name].PID() != 0 {
			log.Printf("[%s] stopping process", name)
			r.backends[name].StopProcess()
		}
	}
}
