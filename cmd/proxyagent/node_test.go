// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/base64"
	"testing"
)

func TestParseVLESSReality(t *testing.T) {
	uri := "vless://11111111-2222-3333-4444-555555555555@203.0.113.7:443" +
		"?encryption=none&security=reality&sni=www.example.com&fp=chrome&pbk=PUBKEY&sid=abcd" +
		"&type=tcp&flow=xtls-rprx-vision#%E9%A6%99%E6%B8%AF01"

	n, err := ParseURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != "vless" || n.Server != "203.0.113.7" || n.Port != 443 {
		t.Fatalf("unexpected node %+v", n)
	}
	if !n.TLS || !n.Reality || n.PublicKey != "PUBKEY" || n.ShortID != "abcd" {
		t.Fatalf("reality fields lost: %+v", n)
	}
	if n.Flow != "xtls-rprx-vision" || n.SNI != "www.example.com" {
		t.Fatalf("stream fields lost: %+v", n)
	}
	if n.Name != "香港01" {
		t.Errorf("fragment should be unescaped, got %q", n.Name)
	}
}

func TestParseVLESSWebSocket(t *testing.T) {
	uri := "vless://uuid-1@example.com:8443?security=tls&sni=a.example.com&type=ws&path=%2Fws&host=a.example.com#ws"
	n, err := ParseURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	if n.Transport != "ws" || n.Path != "/ws" || !n.TLS || n.Port != 8443 {
		t.Fatalf("unexpected node %+v", n)
	}
}

func TestParseVMess(t *testing.T) {
	payload := `{"v":"2","ps":"tokyo","add":"198.51.100.9","port":"443","id":"uuid-2",
		"aid":"0","scy":"auto","net":"ws","type":"none","host":"cdn.example.com",
		"path":"/ray","tls":"tls","sni":"cdn.example.com"}`
	uri := "vmess://" + base64.StdEncoding.EncodeToString([]byte(payload))

	n, err := ParseURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != "vmess" || n.Server != "198.51.100.9" || n.Port != 443 {
		t.Fatalf("unexpected node %+v", n)
	}
	if n.UUID != "uuid-2" || n.Transport != "ws" || !n.TLS || n.Path != "/ray" {
		t.Fatalf("unexpected node %+v", n)
	}
}

func TestParseTrojan(t *testing.T) {
	n, err := ParseURI("trojan://secret@trojan.example.com:443?sni=trojan.example.com#tj")
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != "trojan" || n.Password != "secret" || !n.TLS {
		t.Fatalf("unexpected node %+v", n)
	}
}

func TestParseShadowsocksSIP002(t *testing.T) {
	cred := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pass123"))
	n, err := ParseURI("ss://" + cred + "@ss.example.com:8388#ss1")
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != "shadowsocks" || n.Method != "aes-256-gcm" || n.Password != "pass123" {
		t.Fatalf("unexpected node %+v", n)
	}
	if n.Server != "ss.example.com" || n.Port != 8388 {
		t.Fatalf("unexpected address %+v", n)
	}
}

func TestParseShadowsocksLegacy(t *testing.T) {
	cred := base64.StdEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:pw@ss2.example.com:9999"))
	n, err := ParseURI("ss://" + cred + "#ss2")
	if err != nil {
		t.Fatal(err)
	}
	if n.Method != "chacha20-ietf-poly1305" || n.Server != "ss2.example.com" || n.Port != 9999 {
		t.Fatalf("unexpected node %+v", n)
	}
}

func TestParseRejectsBrokenInput(t *testing.T) {
	for _, uri := range []string{
		"vless://@host:443",
		"trojan://@host:443",
		"ss://not-base64",
		"http://example.com/repo.git",
		"vless://uuid@host",
	} {
		if _, err := ParseURI(uri); err == nil {
			t.Errorf("expected %q to be rejected", uri)
		}
	}
}

func TestParseSubscriptionSkipsUnknown(t *testing.T) {
	raw := "vless://uuid@a.example.com:443?security=tls#a\n" +
		"# a comment\n" +
		"\n" +
		"http://not-a-node\n" +
		"trojan://pw@b.example.com:443#b\n"

	nodes, skipped := ParseSubscription(raw)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	if skipped != 1 {
		t.Errorf("expected 1 skipped line, got %d", skipped)
	}
}

func TestDecodeSubscriptionBase64(t *testing.T) {
	plain := "trojan://pw@b.example.com:443#b"
	encoded := base64.StdEncoding.EncodeToString([]byte(plain))

	if got := DecodeSubscription(encoded); got != plain {
		t.Errorf("base64 body not decoded: %q", got)
	}
	if got := DecodeSubscription(plain); got != plain {
		t.Errorf("plain body must be returned as is: %q", got)
	}
	if got := DecodeSubscription("\ufeff" + plain); got != plain {
		t.Errorf("BOM not stripped: %q", got)
	}
}
