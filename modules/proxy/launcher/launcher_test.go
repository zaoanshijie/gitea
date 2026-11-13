// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package launcher

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"
)

var helperListen = flag.String("launcher-helper-listen", "", "internal: serve a fake socks endpoint")
var helperExit = flag.Bool("launcher-helper-exit", false, "internal: exit immediately")
var helperReady = flag.String("launcher-helper-ready", "", "internal: announce readiness without listening")

// TestHelperSocks doubles as a fake SOCKS5 endpoint when the supervisor spawns
// this very test binary. It is a no-op during a normal test run.
func TestHelperSocks(t *testing.T) {
	if *helperListen == "" {
		return
	}
	ln, err := net.Listen("tcp", *helperListen)
	if err != nil {
		os.Exit(2)
	}
	serveSocks(ln)
}

func TestHelperExit(t *testing.T) {
	if !*helperExit {
		return
	}
	os.Exit(0)
}

// TestHelperReady announces readiness without binding anything, so a test can
// tell the agent's own signal apart from a port that happens to be open.
func TestHelperReady(t *testing.T) {
	if *helperReady == "" {
		return
	}
	fmt.Printf("ready listen=%s node=stand-in\n", *helperReady)
	select {}
}

func serveSocks(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleSocks(conn)
	}
}

// handleSocks performs just enough of the SOCKS5 conversation for a dialer to
// believe the tunnel is open.
func handleSocks(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 512)

	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return
	}
	if _, err := io.ReadFull(conn, buf[:int(buf[1])]); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}

	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		return
	}
	switch buf[3] {
	case 1:
		_, _ = io.ReadFull(conn, buf[:6])
	case 3:
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			return
		}
		_, _ = io.ReadFull(conn, buf[:int(buf[0])+2])
	case 4:
		_, _ = io.ReadFull(conn, buf[:18])
	}
	_, _ = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	time.Sleep(50 * time.Millisecond)
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

func newTestSupervisor(addr string, args ...string) *Supervisor {
	u, _ := url.Parse("socks5://" + addr)
	return &Supervisor{
		cfg: Config{
			Command:      os.Args[0],
			Args:         args,
			ListenAddr:   addr,
			ProxyURL:     u,
			StartTimeout: 10 * time.Second,
			ProbeTimeout: 3 * time.Second,
			ProbeTTL:     time.Minute,
		},
		verified: make(map[string]time.Time),
	}
}

func githubRequest() *http.Request {
	return &http.Request{URL: &url.URL{Scheme: "https", Host: "github.com"}}
}

func TestProxyForWithoutSupervisor(t *testing.T) {
	Shutdown()

	got, err := ProxyFor(githubRequest())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("a disabled launcher must dial directly, got %v", got)
	}
}

func TestSupervisorStartsAndCaches(t *testing.T) {
	addr := freeAddr(t)
	s := newTestSupervisor(addr, "-test.run=^TestHelperSocks$", "-launcher-helper-listen", addr)
	defer s.stop()

	got := s.proxyFor(githubRequest())
	if got == nil {
		t.Fatal("expected the proxy to be used once the endpoint answers")
	}
	if got.String() != "socks5://"+addr {
		t.Errorf("unexpected proxy url %q", got)
	}
	if !s.recentlyVerified("github.com:443") {
		t.Error("a successful probe should be cached")
	}

	// The second call must reuse the cache instead of spawning again.
	first := s.proc
	if second := s.proxyFor(githubRequest()); second == nil {
		t.Fatal("expected the cached proxy to be returned")
	}
	if s.proc != first {
		t.Error("the supervisor should not spawn a second process")
	}
}

func TestSupervisorFallsBackWhenProcessDies(t *testing.T) {
	addr := freeAddr(t)
	s := newTestSupervisor(addr, "-test.run=^TestHelperExit$", "-launcher-helper-exit")
	s.cfg.StartTimeout = 2 * time.Second
	defer s.stop()

	if got := s.proxyFor(githubRequest()); got != nil {
		t.Fatalf("expected a direct connection, got %v", got)
	}
	if !s.coolUntil.After(time.Now()) {
		t.Error("a failed start should trigger a cooldown")
	}
}

func TestSupervisorFallsBackWhenBinaryMissing(t *testing.T) {
	addr := freeAddr(t)
	s := newTestSupervisor(addr, "-test.run=^TestHelperSocks$", "-launcher-helper-listen", addr)
	s.cfg.Command = "/nonexistent/definitely-not-here"

	if got := s.proxyFor(githubRequest()); got != nil {
		t.Fatalf("expected a direct connection, got %v", got)
	}
}

// TestStartTrustsTheReadyLine pins the fix for the retarget storm: readiness
// must come from the agent itself, not from whoever answers on the port.
func TestStartTrustsTheReadyLine(t *testing.T) {
	addr := freeAddr(t)
	s := newTestSupervisor(addr, "-test.run=^TestHelperReady$", "-launcher-helper-ready", addr)
	s.cfg.StartTimeout = 10 * time.Second
	defer s.stop()

	start := time.Now()
	if err := s.startLocked("github.com", "github.com"); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	// Nothing is listening on addr, so only the ready line can end the wait.
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("start returned after %s, it must use the agent's announcement", took)
	}
	if s.proc == nil {
		t.Fatal("expected a running agent")
	}
}

// TestRetargetWaitsForTheOldInstance checks that a host change reaps the old
// agent before the new one starts. Overlapping instances fight over the port
// and the core cache, and the loser used to be killed by the winner's probe.
func TestRetargetWaitsForTheOldInstance(t *testing.T) {
	addr := freeAddr(t)
	s := newTestSupervisor(addr, "-test.run=^TestHelperSocks$", "-launcher-helper-listen", addr)
	defer s.stop()

	if got := s.proxyFor(githubRequest()); got == nil {
		t.Fatal("expected the proxy to be used")
	}
	first := s.proc

	other := &http.Request{URL: &url.URL{Scheme: "https", Host: "gitlab.freedesktop.org"}}
	if got := s.proxyFor(other); got == nil {
		t.Fatal("expected the proxy after retargeting")
	}
	if s.proc == first {
		t.Fatal("retargeting must start a new agent")
	}
	select {
	case <-first.done:
	default:
		t.Error("the previous agent must be reaped before the replacement starts")
	}
}

// TestReapLeavesTheReplacementAlone covers the second half of the race: the
// predecessor's exit used to mark the fresh agent as dead.
func TestReapLeavesTheReplacementAlone(t *testing.T) {
	s := &Supervisor{verified: make(map[string]time.Time)}

	old := &proc{
		cmd:   exec.Command(os.Args[0], "-test.run=^TestHelperExit$", "-launcher-helper-exit"),
		done:  make(chan struct{}),
		ready: make(chan struct{}),
	}
	if err := old.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s.proc = old
	go s.reap(old)

	fresh := &proc{done: make(chan struct{}), ready: make(chan struct{})}
	s.proc = fresh

	select {
	case <-old.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the helper never exited")
	}
	time.Sleep(300 * time.Millisecond)

	if s.proc != fresh {
		t.Error("a reaped predecessor must not clear the agent that replaced it")
	}
}

func TestMatch(t *testing.T) {
	s := &Supervisor{cfg: Config{SkipHosts: []string{"gitee.com", "gitcode.com"}}}
	cases := []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"codeload.github.com", true},
		{"gitee.com", false},
		{"mirror.gitee.com.cn", false},
		{"localhost", false},
		{"127.0.0.1", false},
		{"192.168.15.114", false},
		{"10.0.0.7", false},
		{"fd00::1", false},
		{"8.8.8.8", true},
		{"", false},
	}
	for _, tc := range cases {
		if got := s.match(tc.host); got != tc.want {
			t.Errorf("match(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestMatchAllowList(t *testing.T) {
	s := &Supervisor{cfg: Config{AllowHosts: []string{"github.com", "*.githubusercontent.com"}}}
	cases := map[string]bool{
		"github.com":                    true,
		"api.github.com":                false,
		"raw.githubusercontent.com":     true,
		"objects.githubusercontent.com": true,
		"example.com":                   false,
	}
	for host, want := range cases {
		if got := s.match(host); got != want {
			t.Errorf("match(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestTargetHostPort(t *testing.T) {
	cases := []struct {
		url  string
		host string
		port string
	}{
		{"https://github.com", "github.com", "443"},
		{"http://mirror.local", "mirror.local", "80"},
		{"https://example.com:8443/x", "example.com", "8443"},
	}
	for _, tc := range cases {
		u, _ := url.Parse(tc.url)
		host, port := targetHostPort(&http.Request{URL: u})
		if host != tc.host || port != tc.port {
			t.Errorf("targetHostPort(%s) = %s,%s want %s,%s", tc.url, host, port, tc.host, tc.port)
		}
	}
}

func TestRepoURL(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{
			"https://github.com/bkaradzic/bgfx.git/info/refs?service=git-upload-pack",
			"https://github.com/bkaradzic/bgfx.git",
		},
		{
			"https://git.ffmpeg.org/ffmpeg.git/info/refs?service=git-upload-pack",
			"https://git.ffmpeg.org/ffmpeg.git",
		},
		{
			// LFS and archive downloads are not repository refs, so the caller
			// falls back to the host instead of restarting the agent.
			"https://github.com/a/b.git/info/lfs/objects/batch",
			"",
		},
		{"https://github.com/a/b.git", ""},
		{"https://github.com/info/refs?x=1", ""},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		if got := repoURL(&http.Request{URL: u}); got != tc.want {
			t.Errorf("repoURL(%s) = %q want %q", tc.url, got, tc.want)
		}
	}
}

func TestProbeSOCKS5(t *testing.T) {
	addr := freeAddr(t)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveSocks(ln)

	if err := probeSOCKS5(t.Context(), addr, "github.com:443"); err != nil {
		t.Fatalf("probe through a working endpoint must succeed: %v", err)
	}

	dead := freeAddr(t)
	if err := probeSOCKS5(t.Context(), dead, "github.com:443"); err == nil {
		t.Fatal("probe against a closed port must fail")
	}
}
