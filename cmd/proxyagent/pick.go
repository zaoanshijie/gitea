// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

// result is one probed node.
type result struct {
	node  Node
	delay time.Duration
	err   error
}

// RankNodes probes every candidate concurrently and returns the reachable ones
// ordered by handshake delay. A handshake is cheap, so a whole subscription can
// be walked in a couple of seconds; the winner is then verified through the real
// core before it is trusted.
func RankNodes(ctx context.Context, nodes []Node, timeout, maxDelay time.Duration, concurrency int) ([]Node, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no candidate nodes")
	}
	if concurrency <= 0 {
		concurrency = 16
	}

	results := make([]result, len(nodes))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			delay, err := probeNode(ctx, nodes[i], timeout)
			results[i] = result{node: nodes[i], delay: delay, err: err}
		}(i)
	}
	wg.Wait()

	usable := make([]result, 0, len(results))
	failures := 0
	for _, r := range results {
		if r.err != nil || (maxDelay > 0 && r.delay > maxDelay) {
			failures++
			continue
		}
		usable = append(usable, r)
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("no reachable node (%d/%d failed)", failures, len(nodes))
	}

	sort.SliceStable(usable, func(i, j int) bool { return usable[i].delay < usable[j].delay })

	ranked := make([]Node, len(usable))
	for i, r := range usable {
		ranked[i] = r.node
	}
	return ranked, nil
}

// SelectBestNode keeps the simple "fastest handshake" behaviour.
func SelectBestNode(ctx context.Context, nodes []Node, timeout, maxDelay time.Duration, concurrency int) (Node, error) {
	ranked, err := RankNodes(ctx, nodes, timeout, maxDelay, concurrency)
	if err != nil {
		return Node{}, err
	}
	return ranked[0], nil
}

// probeNode measures how long a TCP (and when applicable TLS) handshake to the
// node takes. It never sends traffic through the node, which keeps the probe
// cheap enough to run against a whole subscription.
func probeNode(ctx context.Context, n Node, timeout time.Duration) (time.Duration, error) {
	start := time.Now()

	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", n.Addr())
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if n.TLS {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         n.SNI,
			InsecureSkipVerify: true, // the endpoint is being measured, not trusted here
			NextProtos:         n.ALPN,
		})
		_ = tlsConn.SetDeadline(time.Now().Add(timeout))
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return 0, err
		}
	}
	return time.Since(start), nil
}
