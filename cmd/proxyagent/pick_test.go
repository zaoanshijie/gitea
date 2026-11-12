// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

func hostPortOf(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portRaw, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func TestSelectBestNodeSkipsUnreachable(t *testing.T) {
	aliveHost, alivePort := hostPortOf(t, echoServer(t))
	deadHost, deadPort := hostPortOf(t, freeAddr(t))

	nodes := []Node{
		{Kind: "trojan", Name: "dead", Server: deadHost, Port: deadPort},
		{Kind: "trojan", Name: "alive", Server: aliveHost, Port: alivePort},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	best, err := SelectBestNode(ctx, nodes, time.Second, 0, 4)
	if err != nil {
		t.Fatalf("expected a node, got %v", err)
	}
	if best.Name != "alive" {
		t.Fatalf("picked %q instead of the reachable node", best.Name)
	}
}

func TestSelectBestNodeAllDead(t *testing.T) {
	host, port := hostPortOf(t, freeAddr(t))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := SelectBestNode(ctx, []Node{{Server: host, Port: port}}, time.Second, 0, 2); err == nil {
		t.Fatal("expected an error when every node is unreachable")
	}
}

func TestSelectBestNodeEmpty(t *testing.T) {
	if _, err := SelectBestNode(context.Background(), nil, time.Second, 0, 2); err == nil {
		t.Fatal("expected an error for an empty list")
	}
}
