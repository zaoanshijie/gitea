// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"testing"
)

type decodedConfig struct {
	Inbounds []struct {
		Type       string `json:"type"`
		Listen     string `json:"listen"`
		ListenPort int    `json:"listen_port"`
	} `json:"inbounds"`
	Outbounds []struct {
		Type       string         `json:"type"`
		Tag        string         `json:"tag"`
		Server     string         `json:"server"`
		ServerPort int            `json:"server_port"`
		UUID       string         `json:"uuid"`
		Flow       string         `json:"flow"`
		Method     string         `json:"method"`
		Password   string         `json:"password"`
		TLS        map[string]any `json:"tls"`
		Transport  map[string]any `json:"transport"`
	} `json:"outbounds"`
	Route struct {
		Final string `json:"final"`
	} `json:"route"`
}

func buildForTest(t *testing.T, node Node) decodedConfig {
	t.Helper()
	raw, err := BuildSingBoxConfig(node, "127.0.0.1:17891", "/tmp/cache.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSingBoxConfig(raw); err != nil {
		t.Fatalf("generated config is invalid: %v", err)
	}
	var cfg decodedConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBuildVLESSReality(t *testing.T) {
	node := Node{
		Kind: "vless", Name: "hk", Server: "203.0.113.7", Port: 443,
		UUID: "uuid-1", Flow: "xtls-rprx-vision",
		TLS: true, SNI: "www.example.com", Reality: true,
		PublicKey: "PUBKEY", ShortID: "abcd", Fingerprint: "chrome",
	}
	cfg := buildForTest(t, node)

	if len(cfg.Inbounds) != 1 || cfg.Inbounds[0].Type != "socks" || cfg.Inbounds[0].ListenPort != 17891 {
		t.Fatalf("unexpected inbound %+v", cfg.Inbounds)
	}
	out := cfg.Outbounds[0]
	if out.Type != "vless" || out.Server != "203.0.113.7" || out.ServerPort != 443 || out.UUID != "uuid-1" {
		t.Fatalf("unexpected outbound %+v", out)
	}
	if out.Flow != "xtls-rprx-vision" {
		t.Errorf("flow lost: %+v", out)
	}
	reality, ok := out.TLS["reality"].(map[string]any)
	if !ok || reality["public_key"] != "PUBKEY" || reality["short_id"] != "abcd" {
		t.Fatalf("reality block wrong: %+v", out.TLS)
	}
	utls, ok := out.TLS["utls"].(map[string]any)
	if !ok || utls["fingerprint"] != "chrome" {
		t.Fatalf("utls block wrong: %+v", out.TLS)
	}
	if cfg.Route.Final != "proxy" {
		t.Errorf("route final = %q", cfg.Route.Final)
	}
}

func TestBuildVMessWebSocket(t *testing.T) {
	node := Node{
		Kind: "vmess", Server: "198.51.100.9", Port: 443, UUID: "uuid-2",
		Security: "auto", Transport: "ws", Path: "/ray", SNI: "cdn.example.com", TLS: true,
	}
	cfg := buildForTest(t, node)
	out := cfg.Outbounds[0]

	if out.Type != "vmess" || out.UUID != "uuid-2" {
		t.Fatalf("unexpected outbound %+v", out)
	}
	if out.Transport["type"] != "ws" || out.Transport["path"] != "/ray" {
		t.Fatalf("ws transport wrong: %+v", out.Transport)
	}
	headers, _ := out.Transport["headers"].(map[string]any)
	if headers["Host"] != "cdn.example.com" {
		t.Errorf("ws Host header = %v", headers)
	}
}

func TestBuildShadowsocks(t *testing.T) {
	node := Node{Kind: "shadowsocks", Server: "ss.example.com", Port: 8388, Method: "aes-256-gcm", Password: "pw"}
	cfg := buildForTest(t, node)
	out := cfg.Outbounds[0]
	if out.Type != "shadowsocks" || out.Method != "aes-256-gcm" || out.Password != "pw" {
		t.Fatalf("unexpected outbound %+v", out)
	}
	if out.TLS != nil {
		t.Errorf("plain node must not carry tls settings")
	}
}

func TestBuildTrojanGRPC(t *testing.T) {
	node := Node{
		Kind: "trojan", Server: "t.example.com", Port: 443, Password: "pw",
		TLS: true, SNI: "t.example.com", Transport: "grpc", ServiceName: "svc",
	}
	cfg := buildForTest(t, node)
	out := cfg.Outbounds[0]
	if out.Type != "trojan" || out.Password != "pw" {
		t.Fatalf("unexpected outbound %+v", out)
	}
	if out.Transport["type"] != "grpc" || out.Transport["service_name"] != "svc" {
		t.Fatalf("grpc transport wrong: %+v", out.Transport)
	}
}

func TestBuildRejectsBadListen(t *testing.T) {
	if _, err := BuildSingBoxConfig(Node{Kind: "trojan"}, "not-an-address", ""); err == nil {
		t.Fatal("expected an error for a malformed listen address")
	}
}
