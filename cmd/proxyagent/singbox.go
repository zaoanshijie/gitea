// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
)

// BuildSingBoxConfig renders a minimal sing-box configuration that exposes one
// local SOCKS inbound and routes it through the chosen node.
func BuildSingBoxConfig(node Node, listenAddr string) ([]byte, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return nil, err
	}
	listenPort, err := strconv.Atoi(port)
	if err != nil {
		return nil, err
	}

	cfg := map[string]any{
		"log": map[string]any{"level": "warn", "timestamp": true},
		"inbounds": []any{
			map[string]any{
				"type":        "socks",
				"tag":         "in",
				"listen":      host,
				"listen_port": listenPort,
			},
		},
		"outbounds": []any{
			buildOutbound(node),
			map[string]any{"type": "direct", "tag": "direct"},
		},
		"route": map[string]any{
			"rules": []any{
				map[string]any{"ip_is_private": true, "outbound": "direct"},
			},
			"final": "proxy",
		},
	}

	return json.MarshalIndent(cfg, "", "  ")
}

func buildOutbound(n Node) map[string]any {
	out := map[string]any{
		"type":        n.Kind,
		"tag":         "proxy",
		"server":      n.Server,
		"server_port": n.Port,
	}

	switch n.Kind {
	case "vless":
		out["uuid"] = n.UUID
		if n.Flow != "" {
			out["flow"] = n.Flow
		}
	case "vmess":
		out["uuid"] = n.UUID
		if n.Security != "" {
			out["security"] = n.Security
		}
		if n.AlterID > 0 {
			out["alter_id"] = n.AlterID
		}
	case "trojan":
		out["password"] = n.Password
	case "shadowsocks":
		out["method"] = n.Method
		out["password"] = n.Password
	}

	if n.TLS {
		out["tls"] = buildTLS(n)
	}
	if transport := buildTransport(n); transport != nil {
		out["transport"] = transport
	}
	return out
}

func buildTLS(n Node) map[string]any {
	tls := map[string]any{"enabled": true}
	if n.SNI != "" {
		tls["server_name"] = n.SNI
	}
	if n.Insecure {
		tls["insecure"] = true
	}
	if len(n.ALPN) > 0 {
		tls["alpn"] = n.ALPN
	}

	// reality needs uTLS, so enable it whenever a fingerprint is known.
	fingerprint := n.Fingerprint
	if fingerprint == "" && n.Reality {
		fingerprint = "chrome"
	}
	if fingerprint != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fingerprint}
	}

	if n.Reality {
		reality := map[string]any{"enabled": true, "public_key": n.PublicKey}
		if n.ShortID != "" {
			reality["short_id"] = n.ShortID
		}
		tls["reality"] = reality
	}
	return tls
}

func buildTransport(n Node) map[string]any {
	switch n.Transport {
	case "ws":
		path := n.Path
		if path == "" {
			path = "/"
		}
		headers := map[string]any{}
		if h := firstNonEmpty(n.Host, n.SNI); h != "" {
			headers["Host"] = h
		}
		return map[string]any{"type": "ws", "path": path, "headers": headers}
	case "grpc":
		service := firstNonEmpty(n.ServiceName, n.Path)
		return map[string]any{"type": "grpc", "service_name": service}
	case "http":
		path := n.Path
		if path == "" {
			path = "/"
		}
		tr := map[string]any{"type": "http", "path": path}
		if h := firstNonEmpty(n.Host, n.SNI); h != "" {
			tr["host"] = []string{h}
		}
		return tr
	default:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ValidateSingBoxConfig is a cheap sanity check used by tests.
func ValidateSingBoxConfig(raw []byte) error {
	var cfg struct {
		Outbounds []struct {
			Type string `json:"type"`
			Tag  string `json:"tag"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if len(cfg.Outbounds) == 0 || cfg.Outbounds[0].Tag != "proxy" {
		return fmt.Errorf("missing proxy outbound")
	}
	return nil
}
