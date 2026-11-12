// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"net"
	"testing"
	"time"
)

func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestRelayForwardsAndTracksUsage(t *testing.T) {
	upstream := echoServer(t)
	listen := freeAddr(t)

	r, err := newRelay(listen, upstream)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()

	conn, err := net.Dial("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("relayed echo failed: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("unexpected echo %q", buf)
	}

	if got := r.activeConnections(); got != 1 {
		t.Errorf("active connections = %d, want 1", got)
	}

	_ = conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for r.activeConnections() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := r.activeConnections(); got != 0 {
		t.Errorf("active connections after close = %d, want 0", got)
	}
}

func TestRelayIdleGrowsWhenUnused(t *testing.T) {
	upstream := echoServer(t)
	listen := freeAddr(t)

	r, err := newRelay(listen, upstream)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()

	first := r.idleFor()
	time.Sleep(120 * time.Millisecond)
	if second := r.idleFor(); second <= first {
		t.Errorf("idle time did not grow: %s -> %s", first, second)
	}
}
