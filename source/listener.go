/* Copyright (C) 2018 KIM KeepInMind GmbH/srl

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

// Package listener provides a functionalities to discover and inspect
// sources.
package source

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/booster-proj/booster/core"
	"upspin.io/log"
)

// Store describes an entity that is able to store,
// delete and list sources.
type Store interface {
	Put(...core.Source)
	Del(...core.Source)
	GetActive() []core.Source
}

// Provider describes a service that is capable of providing sources
// and checking their effective internet connection using a defined
// level of confidence.
type Provider interface {
	Provide(context.Context) ([]core.Source, error)
	Check(context.Context, core.Source, Confidence) error
}

type Listener struct {
	// Source provider.
	Provider

	// The location where the active sources are stored.
	s Store
	// Hook errors handler.
	h *Hooker
}

var PollInterval = time.Second * 3
var PollTimeout = time.Second * 5

type Config struct {
	Store           Store
	Provider        Provider
	MetricsExporter MetricsExporter
}

// NewListener creates a new Listener with the provided storage, using
// as Provider the MergedProvider implementation.
func NewListener(c Config) *Listener {
	hooker := &Hooker{hooked: make(map[string]*hookErr)}

	var p Provider = &MergedProvider{
		ControlInterface: func(ifi *Interface) {
			ifi.OnDialErr = hooker.HandleDialErr
			ifi.SetMetricsExporter(c.MetricsExporter)
		},
	}
	if c.Provider != nil {
		p = c.Provider
	}

	return &Listener{
		s:        c.Store,
		h:        hooker,
		Provider: p,
	}
}

type hookErr struct {
	receivedAt time.Time
	ref        string
	network    string
	address    string
	err        error
}

func (err *hookErr) Error() string {
	return fmt.Sprintf("error %v produced by source %s while contacting %s using %s", err.err, err.ref, err.address, err.network)
}

type Hooker struct {
	sync.Mutex
	hooked map[string]*hookErr // list of hook errors mapped by source Name
}

func (h *Hooker) HandleDialErr(ref, network, address string, err error) {
	log.Debug.Printf("Listener: ErrHook called from %s (net: %s, addr: %s): %v", ref, network, address, err)

	hookErr := &hookErr{
		receivedAt: time.Now(),
		ref:        ref,
		network:    network,
		err:        err,
	}
	h.Add(hookErr)
}

func (h *Hooker) Add(err *hookErr) {
	h.Lock()
	if h.hooked == nil {
		h.hooked = make(map[string]*hookErr)
	}
	h.hooked[err.ref] = err
	h.Unlock()
}

func (h *Hooker) HookErr(id string) error {
	h.Lock()
	defer h.Unlock()

	if err, ok := h.hooked[id]; ok && err != nil {
		delete(h.hooked, id) // cleanup, the error must be handled now.
		return err
	}
	return nil
}

// Run is a blocking function which keeps on calling Poll and waiting
// PollInterval amount of time. This function will stop with an error
// only in case of a context cancelation and in case that the Poll
// function returns with a critical error.
func (l *Listener) Run(ctx context.Context) error {
	for {
		_ctx, cancel := context.WithTimeout(ctx, PollTimeout)
		defer cancel()

		if err := l.Poll(_ctx); err != nil {
			// Just log the error
			log.Error.Println(err)
		}

		select {
		case <-ctx.Done():
			// Exit in case of context cancelation.
			return ctx.Err()
		case <-time.After(PollInterval):
			// Wait before polling again.
		}
	}
}

// Diff returns respectively the list of items that has to be added and removed
// from "old" to create the same list as "cur".
func Diff(old, cur []core.Source) (add []core.Source, remove []core.Source) {
	oldm := make(map[string]core.Source, len(old))
	curm := make(map[string]core.Source, len(cur))
	for _, v := range old {
		oldm[v.Name()] = v
	}
	for _, v := range cur {
		curm[v.Name()] = v
	}

	for _, v := range old {
		// find sources to remove
		if _, ok := curm[v.Name()]; !ok {
			remove = append(remove, v)
		}
	}

	for _, v := range cur {
		// find sources to add
		if _, ok := oldm[v.Name()]; !ok {
			add = append(add, v)
		}
	}

	return
}

// Poll queries the provider for a list of sources. It then inspect each
// new source, saving into the storage the sources that provide an active
// internet connection and removing the ones that are no longer available.
func (l *Listener) Poll(ctx context.Context) error {
	// Fetch new & old data
	cur, err := l.Provide(ctx)
	if err != nil {
		return err
	}

	old := l.s.GetActive()

	// Find difference from old to cur.
	add, remove := Diff(old, cur)

	// Inspect the new ones, add them if they provide an internet connection.
	for _, v := range add {
		log.Debug.Printf("Poll: add %v?", v)
		if err := l.Check(ctx, v, High); err != nil {
			log.Debug.Printf("Poll: unable to add source: %v", err)
			continue
		}
		// New source WITH active internet connection found!
		log.Info.Printf("Listener: adding (%v) to storage.", v)
		l.s.Put(v)
	}

	// Remove what has to be removed without further investigation
	for _, v := range remove {
		log.Info.Printf("Listener: removing (%v) from storage.", v)
		l.s.Del(v)
		_ = l.h.HookErr(v.Name()) // also consume hook errors.
	}

	// Eventually remove the sources that contain hook errors.
	old = l.s.GetActive() // as the list has been updated before the last call.
	acc := make([]core.Source, 0, len(old))
	for _, src := range old {
		if err = l.h.HookErr(src.Name()); err != nil {
			// This source has an hook error.
			acc = append(acc, src)
		}
	}
	for _, v := range acc {
		// We collected a hook error. This does not mean that the source does
		// not provide an internet connection.
		if err := l.Check(ctx, v, High); err != nil {
			log.Info.Printf("Listener: removing (%v) from storage after hook error.", v)
			l.s.Del(v)
		}
	}

	return nil
}
