// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// relay forwards the port Gitea talks to onto the core the agent supervises.
// It exists so the agent can tell whether anybody is still using it: a SOCKS
// server cannot report that, a relay can.
type relay struct {
	ln     net.Listener
	target string

	active     atomic.Int64
	lastActive atomic.Int64 // unix nanoseconds

	closeOnce sync.Once
	wg        sync.WaitGroup
}

func newRelay(listen, target string) (*relay, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	r := &relay{ln: ln, target: target}
	r.touch()
	go r.serve()
	return r, nil
}

func (r *relay) serve() {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return
		}
		r.active.Add(1)
		r.touch()
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer func() {
				r.active.Add(-1)
				r.touch()
			}()
			r.handle(conn)
		}()
	}
}

func (r *relay) handle(client net.Conn) {
	defer client.Close()

	upstream, err := net.DialTimeout("tcp", r.target, 10*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, client)
		closeWrite(upstream)
		close(done)
	}()
	_, _ = io.Copy(client, upstream)
	closeWrite(client)
	<-done
}

func (r *relay) touch() {
	r.lastActive.Store(time.Now().UnixNano())
}

// activeConnections reports how many tunnels are open right now.
func (r *relay) activeConnections() int64 { return r.active.Load() }

// idleFor reports how long nothing has been opened or closed.
func (r *relay) idleFor() time.Duration {
	return time.Since(time.Unix(0, r.lastActive.Load()))
}

func (r *relay) close() {
	r.closeOnce.Do(func() {
		_ = r.ln.Close()
		done := make(chan struct{})
		go func() {
			r.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
}

func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}
