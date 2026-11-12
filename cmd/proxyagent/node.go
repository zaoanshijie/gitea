// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Node is one proxy endpoint taken from a subscription, normalised so that
// sing-box's configuration can be generated without caring about URI dialects.
type Node struct {
	Kind string // vless, vmess, trojan, shadowsocks
	Name string

	Server string
	Port   int

	UUID     string
	Flow     string
	Security string // vmess cipher
	AlterID  int

	Password string
	Method   string // shadowsocks cipher

	Transport   string // tcp, ws, grpc, http
	Path        string
	Host        string
	ServiceName string

	TLS         bool
	SNI         string
	ALPN        []string
	Insecure    bool
	Reality     bool
	PublicKey   string
	ShortID     string
	Fingerprint string
}

// Addr is the dial address used by the latency probe.
func (n Node) Addr() string { return fmt.Sprintf("%s:%d", n.Server, n.Port) }

// String never contains credentials, so it is safe to log.
func (n Node) String() string {
	return fmt.Sprintf("%s(%s, %s)", n.Name, n.Kind, n.Addr())
}

// ParseSubscription returns every endpoint it can understand and the number of
// lines it had to skip.
func ParseSubscription(raw string) ([]Node, int) {
	lines := strings.Split(raw, "\n")
	nodes := make([]Node, 0, len(lines))
	skipped := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		node, err := ParseURI(line)
		if err != nil {
			skipped++
			continue
		}
		nodes = append(nodes, node)
	}
	return nodes, skipped
}

// ParseURI understands the URI schemes that domestic subscriptions actually use.
func ParseURI(uri string) (Node, error) {
	switch {
	case strings.HasPrefix(uri, "vless://"):
		return parseVLESS(uri)
	case strings.HasPrefix(uri, "vmess://"):
		return parseVMess(uri)
	case strings.HasPrefix(uri, "trojan://"):
		return parseTrojan(uri)
	case strings.HasPrefix(uri, "ss://"):
		return parseShadowsocks(uri)
	default:
		return Node{}, fmt.Errorf("unsupported scheme")
	}
}

func parseVLESS(uri string) (Node, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return Node{}, err
	}
	node, err := baseFromURL(u, "vless")
	if err != nil {
		return Node{}, err
	}
	node.UUID = u.User.Username()
	if node.UUID == "" {
		return Node{}, fmt.Errorf("vless without uuid")
	}
	q := u.Query()
	node.Flow = q.Get("flow")
	applyStream(&node, q)
	applySecurity(&node, q, true)
	return node, validateNode(node)
}

func parseTrojan(uri string) (Node, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return Node{}, err
	}
	node, err := baseFromURL(u, "trojan")
	if err != nil {
		return Node{}, err
	}
	node.Password = u.User.Username()
	if node.Password == "" {
		return Node{}, fmt.Errorf("trojan without password")
	}
	q := u.Query()
	applyStream(&node, q)
	applySecurity(&node, q, true)
	node.TLS = true // trojan is TLS-only in practice
	return node, validateNode(node)
}

func parseShadowsocks(uri string) (Node, error) {
	body := strings.TrimPrefix(uri, "ss://")
	name := ""
	if i := strings.IndexByte(body, '#'); i >= 0 {
		name, _ = url.QueryUnescape(body[i+1:])
		body = body[:i]
	}
	body = strings.SplitN(body, "?", 2)[0]

	var method, password, hostport string
	if at := strings.LastIndexByte(body, '@'); at >= 0 {
		// SIP002: ss://base64(method:password)@host:port
		cred, err := decodeBase64(body[:at])
		if err != nil {
			return Node{}, fmt.Errorf("ss credentials: %w", err)
		}
		parts := strings.SplitN(cred, ":", 2)
		if len(parts) != 2 {
			return Node{}, fmt.Errorf("ss credentials malformed")
		}
		method, password, hostport = parts[0], parts[1], body[at+1:]
	} else {
		// Legacy: ss://base64(method:password@host:port)
		decoded, err := decodeBase64(body)
		if err != nil {
			return Node{}, fmt.Errorf("ss body: %w", err)
		}
		at := strings.LastIndexByte(decoded, '@')
		if at < 0 {
			return Node{}, fmt.Errorf("ss body malformed")
		}
		parts := strings.SplitN(decoded[:at], ":", 2)
		if len(parts) != 2 {
			return Node{}, fmt.Errorf("ss credentials malformed")
		}
		method, password, hostport = parts[0], parts[1], decoded[at+1:]
	}

	host, port, err := splitHostPort(hostport)
	if err != nil {
		return Node{}, err
	}
	node := Node{Kind: "shadowsocks", Name: name, Server: host, Port: port, Method: method, Password: password}
	return node, validateNode(node)
}

type vmessJSON struct {
	PS   string `json:"ps"`
	Add  string `json:"add"`
	Port any    `json:"port"`
	ID   string `json:"id"`
	AID  any    `json:"aid"`
	Scy  string `json:"scy"`
	Net  string `json:"net"`
	Type string `json:"type"`
	Host string `json:"host"`
	Path string `json:"path"`
	TLS  string `json:"tls"`
	SNI  string `json:"sni"`
	ALPN string `json:"alpn"`
	FP   string `json:"fp"`
}

func parseVMess(uri string) (Node, error) {
	payload := strings.TrimPrefix(uri, "vmess://")
	if i := strings.IndexByte(payload, '#'); i >= 0 {
		payload = payload[:i]
	}
	decoded, err := decodeBase64(payload)
	if err != nil {
		return Node{}, fmt.Errorf("vmess body: %w", err)
	}
	var v vmessJSON
	if err := json.Unmarshal([]byte(decoded), &v); err != nil {
		return Node{}, fmt.Errorf("vmess json: %w", err)
	}
	port, err := toInt(v.Port)
	if err != nil {
		return Node{}, err
	}
	node := Node{
		Kind:      "vmess",
		Name:      v.PS,
		Server:    v.Add,
		Port:      port,
		UUID:      v.ID,
		Security:  v.Scy,
		AlterID:   toIntOr(v.AID, 0),
		Transport: normalizeTransport(v.Net),
		Path:      v.Path,
		Host:      v.Host,
		SNI:       v.SNI,
		TLS:       v.TLS == "tls",
	}
	if node.SNI == "" {
		node.SNI = v.Host
	}
	if v.ALPN != "" {
		node.ALPN = strings.Split(v.ALPN, ",")
	}
	if v.FP != "" {
		node.Fingerprint = v.FP
	}
	if node.Security == "" {
		node.Security = "auto"
	}
	return node, validateNode(node)
}

func baseFromURL(u *url.URL, kind string) (Node, error) {
	host, port, err := splitHostPort(u.Host)
	if err != nil {
		return Node{}, err
	}
	name := u.Fragment
	if name != "" {
		if unescaped, err := url.QueryUnescape(name); err == nil {
			name = unescaped
		}
	}
	return Node{Kind: kind, Name: name, Server: host, Port: port}, nil
}

func applyStream(node *Node, q url.Values) {
	node.Transport = normalizeTransport(q.Get("type"))
	node.Path = q.Get("path")
	node.Host = q.Get("host")
	node.ServiceName = q.Get("serviceName")
}

func applySecurity(node *Node, q url.Values, allowReality bool) {
	security := strings.ToLower(q.Get("security"))
	node.SNI = q.Get("sni")
	node.Fingerprint = q.Get("fp")
	if alpn := q.Get("alpn"); alpn != "" {
		node.ALPN = strings.Split(alpn, ",")
	}
	node.Insecure = q.Get("allowInsecure") == "1" || strings.EqualFold(q.Get("allowInsecure"), "true")

	switch security {
	case "tls":
		node.TLS = true
	case "reality":
		if !allowReality {
			return
		}
		node.TLS = true
		node.Reality = true
		node.PublicKey = q.Get("pbk")
		node.ShortID = q.Get("sid")
		if node.Fingerprint == "" {
			node.Fingerprint = "chrome"
		}
	}
	if node.SNI == "" {
		node.SNI = q.Get("host")
	}
	if node.SNI == "" && node.TLS {
		node.SNI = node.Server
	}
}

func normalizeTransport(t string) string {
	switch strings.ToLower(t) {
	case "ws", "websocket":
		return "ws"
	case "grpc":
		return "grpc"
	case "h2", "http":
		return "http"
	default:
		return "tcp"
	}
}

func splitHostPort(hostport string) (string, int, error) {
	hostport = strings.TrimPrefix(strings.TrimSpace(hostport), "//")
	i := strings.LastIndexByte(hostport, ':')
	if i < 0 {
		return "", 0, fmt.Errorf("missing port in %q", hostport)
	}
	host := strings.Trim(hostport[:i], "[]")
	port, err := strconv.Atoi(hostport[i+1:])
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port in %q", hostport)
	}
	if host == "" {
		return "", 0, fmt.Errorf("missing host in %q", hostport)
	}
	return host, port, nil
}

func validateNode(n Node) error {
	if n.Server == "" || n.Port == 0 {
		return fmt.Errorf("node %q has no address", n.Name)
	}
	if n.Reality && n.PublicKey == "" {
		return fmt.Errorf("node %q uses reality without a public key", n.Name)
	}
	return nil
}

func decodeBase64(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("-", "+", "_", "/").Replace(s)
	s = strings.TrimRight(s, "=")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("not base64")
}

func toInt(v any) (int, error) {
	switch value := v.(type) {
	case float64:
		return int(value), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, fmt.Errorf("invalid port %q", value)
		}
		return n, nil
	case nil:
		return 0, fmt.Errorf("missing port")
	default:
		return 0, fmt.Errorf("unsupported port type")
	}
}

func toIntOr(v any, def int) int {
	n, err := toInt(v)
	if err != nil {
		return def
	}
	return n
}
