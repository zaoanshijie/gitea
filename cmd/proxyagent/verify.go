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
