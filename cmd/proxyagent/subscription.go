// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxSubscriptionSize = 8 << 20

// FetchSubscription downloads a subscription and returns the endpoints it
// holds. Subscriptions come either as a plain list of URIs or as a single
// base64 blob, both are handled.
func FetchSubscription(ctx context.Context, url string, timeout time.Duration) ([]Node, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "gitea-proxy-agent/1")
	req.Header.Set("Accept", "*/*")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch subscription: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch subscription: unexpected status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionSize))
	if err != nil {
		return nil, fmt.Errorf("read subscription: %w", err)
	}

	nodes, skipped := ParseSubscription(DecodeSubscription(string(body)))
	if len(nodes) == 0 {
		return nil, fmt.Errorf("subscription held no usable node (%d lines skipped)", skipped)
	}
	return nodes, nil
}

// DecodeSubscription unwraps the base64 form used by most providers and strips
// a possible UTF-8 BOM.
func DecodeSubscription(body string) string {
	body = strings.TrimPrefix(body, "\ufeff")
	body = strings.TrimSpace(body)
	if body == "" {
		return body
	}
	if strings.Contains(body, "://") {
		return body
	}
	if decoded, err := decodeBase64(body); err == nil && strings.Contains(decoded, "://") {
		return decoded
	}
	return body
}
