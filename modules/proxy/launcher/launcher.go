// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package launcher

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitea.dev/modules/log"

	"golang.org/x/net/proxy"
)

// Supervisor owns at most one on-demand proxy process.
//
// The process is started the first time a git remote that is not skipped has to
// be reached, and the agent terminates itself once it has been idle long
// enough. Nothing runs while Gitea is idle.
type Supervisor struct {
	cfg Config

	// startMu serialises cold starts and probes so a burst of mirror jobs
	// spawns exactly one process.
	startMu   sync.Mutex
	cmd       *exec.Cmd
	exited    atomic.Bool
	coolUntil time.Time
	lastLogAt time.Time
	lastLog   string

	cacheMu  sync.RWMutex
	verified map[string]time.Time
}

var global atomic.Pointer[Supervisor]

// Init installs the global supervisor from the environment.
//
// It never fails: a missing or invalid configuration simply leaves the global
// supervisor unset, which means every caller keeps the official behaviour.
func Init(ctx context.Context) {
	cfg, enabled, err := Load()
	if err != nil {
		log.Warn("Proxy agent disabled, git egress keeps the official behaviour: %v", err)
		return
	}
	if !enabled {
		return
	}

	s := &Supervisor{cfg: *cfg, verified: make(map[string]time.Time)}
	global.Store(s)
	context.AfterFunc(ctx, s.stop)

	log.Info("Proxy agent enabled: command=%q listen=%s start-timeout=%s idle=%s",
		cfg.Command, cfg.ListenAddr, cfg.StartTimeout, cfg.Idle)
	if len(cfg.SkipHosts) > 0 {
		log.Info("Proxy agent skips: %s", strings.Join(cfg.SkipHosts, ", "))
	}
}

// Enabled reports whether a supervisor is installed.
func Enabled() bool { return global.Load() != nil }

// Shutdown stops the proxy process if it is running.
func Shutdown() {
	if s := global.Swap(nil); s != nil {
		s.stop()
	}
}

// ProxyFor is the hook handed to the egress policy.
//
// A nil URL means "dial directly", which is both the disabled case and the
// fallback case. It never returns an error so an unhealthy proxy can never turn
// a working git remote into a failure.
func ProxyFor(req *http.Request) (*url.URL, error) {
	s := global.Load()
	if s == nil || req == nil || req.URL == nil {
		return nil, nil
	}
	return s.proxyFor(req), nil
}

func (s *Supervisor) proxyFor(req *http.Request) *url.URL {
	host, port := targetHostPort(req)
	if host == "" || !s.match(host) {
		return nil
	}
	key := net.JoinHostPort(host, port)

	if s.recentlyVerified(key) {
		return s.cfg.ProxyURL
	}

	s.startMu.Lock()
	defer s.startMu.Unlock()

	// Another request may have finished the probe while we waited.
	if s.recentlyVerified(key) {
		return s.cfg.ProxyURL
	}

	if err := s.ensureRunningLocked(); err != nil {
		s.warnThrottled("start", err)
		return nil
	}

	probeCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ProbeTimeout)
	defer cancel()
	if err := probeSOCKS5(probeCtx, s.cfg.ListenAddr, key); err != nil {
		s.warnThrottled("probe", fmt.Errorf("%s: %w", key, err))
		s.stopLocked()
		return nil
	}

	s.markVerified(key)
	return s.cfg.ProxyURL
}

func (s *Supervisor) ensureRunningLocked() error {
	if s.cmd != nil && !s.exited.Load() {
		if conn, err := net.DialTimeout("tcp", s.cfg.ListenAddr, time.Second); err == nil {
			_ = conn.Close()
			return nil
		}
		// Process is alive but no longer serving; recycle it.
		s.stopLocked()
	}
	if time.Now().Before(s.coolUntil) {
		return fmt.Errorf("cooling down after a failed start")
	}
	return s.startLocked()
}

func (s *Supervisor) startLocked() error {
	args := make([]string, 0, len(s.cfg.Args))
	for _, a := range s.cfg.Args {
		a = strings.ReplaceAll(a, "{addr}", s.cfg.ListenAddr)
		a = strings.ReplaceAll(a, "{idle}", s.cfg.Idle.String())
		args = append(args, a)
	}

	cmd := exec.Command(s.cfg.Command, args...)
	cmd.Env = os.Environ()
	cmd.Stdout = &lineWriter{prefix: "proxy agent"}
	cmd.Stderr = &lineWriter{prefix: "proxy agent"}
	setProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		s.coolDown()
		return fmt.Errorf("exec %s: %w", s.cfg.Command, err)
	}

	s.cmd = cmd
	s.exited.Store(false)
	go s.reap(cmd)

	log.Info("Proxy agent starting: pid=%d", cmd.Process.Pid)

	deadline := time.Now().Add(s.cfg.StartTimeout)
	for time.Now().Before(deadline) {
		if s.exited.Load() {
			s.coolDown()
			return fmt.Errorf("process exited during startup")
		}
		if conn, err := net.DialTimeout("tcp", s.cfg.ListenAddr, 500*time.Millisecond); err == nil {
			_ = conn.Close()
			log.Info("Proxy agent ready on %s", s.cfg.ListenAddr)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	s.stopLocked()
	s.coolDown()
	return fmt.Errorf("not listening on %s within %s", s.cfg.ListenAddr, s.cfg.StartTimeout)
}

func (s *Supervisor) reap(cmd *exec.Cmd) {
	err := cmd.Wait()
	s.exited.Store(true)
	s.clearVerified()
	if err != nil {
		log.Warn("Proxy agent exited: %v", err)
	} else {
		log.Info("Proxy agent exited after being idle")
	}
}

func (s *Supervisor) stop() {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.stopLocked()
}

func (s *Supervisor) stopLocked() {
	cmd := s.cmd
	if cmd == nil || cmd.Process == nil || s.exited.Load() {
		s.cmd = nil
		return
	}
	terminate(cmd)
	s.cmd = nil
	s.clearVerified()
}

func (s *Supervisor) coolDown() {
	s.coolUntil = time.Now().Add(30 * time.Second)
}

func (s *Supervisor) recentlyVerified(key string) bool {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	at, ok := s.verified[key]
	return ok && time.Since(at) < s.cfg.ProbeTTL
}

func (s *Supervisor) markVerified(key string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.verified == nil {
		s.verified = make(map[string]time.Time)
	}
	s.verified[key] = time.Now()
}

func (s *Supervisor) clearVerified() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.verified = make(map[string]time.Time)
}

// warnThrottled keeps a flapping proxy from flooding the log, which matters
// when a single cron run walks hundreds of mirrors.
func (s *Supervisor) warnThrottled(stage string, err error) {
	msg := stage + ": " + err.Error()
	now := time.Now()
	if msg == s.lastLog && now.Sub(s.lastLogAt) < time.Minute {
		return
	}
	s.lastLog, s.lastLogAt = msg, now
	log.Warn("Proxy agent unavailable (%s), falling back to a direct connection", msg)
}

// match decides whether a host is worth proxying.
func (s *Supervisor) match(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return false
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		addr = addr.Unmap()
		// Internal mirrors and loopback are never reachable through a remote
		// proxy, so proxying them would only break them.
		if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() {
			return false
		}
	}
	// Skip patterns are keywords: "gitee.com" also covers "gitee.com.cn".
	for _, skip := range s.cfg.SkipHosts {
		if strings.Contains(h, skip) {
			return false
		}
	}
	if len(s.cfg.AllowHosts) == 0 {
		return true
	}
	for _, allow := range s.cfg.AllowHosts {
		if hostPatternMatch(allow, h) {
			return true
		}
	}
	return false
}

func hostPatternMatch(pattern, host string) bool {
	if pattern == host {
		return true
	}
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+rest)
	}
	return false
}

func targetHostPort(req *http.Request) (host, port string) {
	host = req.URL.Hostname()
	if host == "" {
		return "", ""
	}
	port = req.URL.Port()
	if port == "" {
		if req.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return host, port
}

// probeSOCKS5 opens a tunnel to target through the on-demand proxy. A SOCKS5
// reply only arrives once the upstream connection is established, so a success
// here means the proxy can really reach the remote, which is what a plain port
// check cannot tell.
func probeSOCKS5(ctx context.Context, proxyAddr, target string) error {
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, &net.Dialer{Timeout: time.Second})
	if err != nil {
		return err
	}
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return fmt.Errorf("socks5 dialer does not support context")
	}
	conn, err := ctxDialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return err
	}
	return conn.Close()
}

// lineWriter forwards the agent's own output into the Gitea log; without it a
// failed start is a black box.
type lineWriter struct {
	prefix string
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		if line != "" {
			log.Info("[%s] %s", w.prefix, line)
		}
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 8192 {
		w.buf = nil
	}
	return len(p), nil
}

var _ io.Writer = (*lineWriter)(nil)
