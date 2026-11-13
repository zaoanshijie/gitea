// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/proxy"
)

// verifyThroughProxy sends a real request through the freshly started core.
//
// A handshake proves the endpoint is awake, not that it can carry traffic: a
// node that answers TLS in 40ms may still time out on the first HTTPS request.
// This check is what makes "pick the fastest" actually usable.
func verifyThroughProxy(ctx context.Context, socksAddr, target string, timeout time.Duration) error {
	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, &net.Dialer{Timeout: timeout})
	if err != nil {
		return err
	}
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return fmt.Errorf("socks5 dialer does not support context")
	}

	transport := &http.Transport{
		DialContext:         ctxDialer.DialContext,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: timeout,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gitea-proxy-agent/1")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 500 {
		return fmt.Errorf("upstream answered %s", resp.Status)
	}
	return nil
}

// probeSpeed measures how much a node can actually carry towards one host.
//
// Several connections are opened at once on purpose: both git and browsers use
// parallel streams, and a single TCP connection is often capped by its window
// long before the node is saturated. Measuring the aggregate is also much
// faster than waiting for one stream to finish.
//
// A failed sample is reported as an error; callers must treat that as "speed
// unknown" rather than "node broken", because a host may simply refuse the
// request while still serving git.
func probeSpeed(ctx context.Context, socksAddr, target string, window time.Duration, conns int, maxBytes int64) (int64, error) {
	if conns < 1 {
		conns = 1
	}
	perConn := maxBytes
	if perConn <= 0 {
		perConn = 4 << 20
	}

	// The window bounds the transfer, not the handshake: a node whose first
	// byte takes 4s can still be the fastest one for a multi-hundred-MB clone.
	probeCtx, cancel := context.WithTimeout(ctx, window+8*time.Second)
	defer cancel()

	type result struct {
		n   int64
		err error
	}
	results := make(chan result, conns)
	start := time.Now()

	for i := 0; i < conns; i++ {
		go func(idx int) {
			n, err := readThroughProxy(probeCtx, socksAddr, target, perConn, idx, window)
			results <- result{n: n, err: err}
		}(i)
	}

	var total int64
	var ok int
	for i := 0; i < conns; i++ {
		r := <-results
		if r.err == nil {
			ok++
			total += r.n
		}
	}

	elapsed := time.Since(start)
	if elapsed <= 0 {
		return 0, nil
	}
	if ok == 0 {
		return 0, fmt.Errorf("no connection could read from %s", target)
	}
	return int64(float64(total) / elapsed.Seconds()), nil
}

// probeAny tries the URLs in order and returns the first measurement that moved
// bytes. This keeps hosts whose root answers with a redirect usable: they are
// simply probed over the other scheme.
func probeAny(ctx context.Context, socksAddr string, urls []string, window time.Duration, conns int, maxBytes int64) (int64, string, error) {
	var lastErr error
	for _, u := range urls {
		bps, err := probeSpeed(ctx, socksAddr, u, window, conns, maxBytes)
		if err == nil && bps > 0 {
			return bps, u, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("%s returned no data", u)
		}
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no probe url returned data")
	}
	return 0, "", lastErr
}

// readThroughProxy copies one stream through the node and returns the bytes it
// managed to read before the deadline.
func readThroughProxy(ctx context.Context, socksAddr, target string, limit int64, idx int, window time.Duration) (int64, error) {
	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, &net.Dialer{Timeout: 5 * time.Second})
	if err != nil {
		return 0, err
	}
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return 0, fmt.Errorf("socks5 dialer does not support context")
	}

	transport := &http.Transport{
		DialContext:         ctxDialer.DialContext,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	defer transport.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "gitea-proxy-agent/1")
	req.Header.Set("Accept-Encoding", "identity")
	// Ask for separate byte ranges so the connections do not all pull the same
	// prefix; hosts that ignore Range still answer with the whole object.
	if idx > 0 {
		start := int64(idx) * limit
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+limit-1))
	}

	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("upstream answered %s", resp.Status)
	}

	// Sample exactly one window worth of body bytes, starting after the first
	// byte arrives so a slow TLS handshake does not hide a fast transfer.
	timer := time.AfterFunc(window, func() { _ = resp.Body.Close() })
	defer timer.Stop()
	return io.Copy(io.Discard, io.LimitReader(resp.Body, limit))
}
