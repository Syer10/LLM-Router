// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"log"
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
	if r.jobs != nil {
		r.jobs.start(ctx)
	}
}

// idleLoop frees the GPU of idle backends: after idle_unload it releases the
// backend's GPU (unloads the model, or stops an owned process), and after
// idle_stop it kills an owned process.
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
				if !bc.UsesGPU() {
					continue
				}
				idle := now.Sub(b.LastActivity())

				// idle_unload is koboldcpp-only: it unloads the model (frees VRAM,
				// keeps the process). For other backends the process is the model,
				// so idle_stop (below) is what frees the GPU.
				if bc.Type == "koboldcpp" && bc.IdleUnload > 0 && idle >= bc.IdleUnload && b.GPUActive() {
					log.Printf("[%s] idle for %s, unloading model", name, idle.Round(time.Second))
					go b.FreeGPU(context.Background())
				}
				if bc.Command != "" && bc.IdleStop > 0 && idle >= bc.IdleStop && b.PID() != 0 {
					log.Printf("[%s] idle for %s, stopping process", name, idle.Round(time.Second))
					b.StopProcess()
				}
			}
		}
	}
}

// StopAll stops every running job and owned process (shutdown path).
func (r *Router) StopAll() {
	if r.jobs != nil {
		r.jobs.stopAll()
	}
	for _, name := range r.order {
		if r.backends[name].PID() != 0 {
			log.Printf("[%s] stopping process", name)
			r.backends[name].StopProcess()
		}
	}
}
