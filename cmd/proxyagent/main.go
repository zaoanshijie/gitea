// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Command gitea-proxy-agent brings up a local SOCKS proxy on demand.
//
// Every start fetches the subscription, ranks its endpoints and keeps the
// fastest one that also survives a real request. The process shuts itself down
// once nothing has used it for a while, so Gitea only pays for a proxy while a
// mirror job is actually running.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type options struct {
	listen       string
	subscription string
	core         string
	checkURL     string

	idle          time.Duration
	maxDelay      time.Duration
	probeTimeout  time.Duration
	subTimeout    time.Duration
	startTimeout  time.Duration
	pickTimeout   time.Duration
	verifyTimeout time.Duration
	concurrency   int
	tryCandidates int
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("proxy-agent: ")

	opts := parseFlags()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, opts); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.listen, "listen", "127.0.0.1:17890", "loopback address to serve SOCKS on")
	flag.StringVar(&o.subscription, "subscription", os.Getenv("GITEA_PROXY_AGENT_SUBSCRIPTION"), "subscription URL, fetched on every start")
	flag.StringVar(&o.core, "core", envOr("GITEA_PROXY_AGENT_CORE", "sing-box"), "path of the sing-box binary")
	flag.StringVar(&o.checkURL, "check-url", "https://github.com/robots.txt", "URL used to verify the chosen node, empty disables the check")
	flag.DurationVar(&o.idle, "idle", 90*time.Second, "exit after this long without any connection (0 disables)")
	flag.DurationVar(&o.maxDelay, "max-delay", 5*time.Second, "drop nodes slower than this handshake")
	flag.DurationVar(&o.probeTimeout, "probe-timeout", 3*time.Second, "per node handshake timeout")
	flag.DurationVar(&o.subTimeout, "subscription-timeout", 20*time.Second, "subscription fetch timeout")
	flag.DurationVar(&o.startTimeout, "start-timeout", 15*time.Second, "how long sing-box may take to bind")
	flag.DurationVar(&o.pickTimeout, "pick-timeout", 45*time.Second, "overall budget for fetch + rank")
	flag.DurationVar(&o.verifyTimeout, "verify-timeout", 12*time.Second, "how long the verification request may take")
	flag.IntVar(&o.concurrency, "concurrency", 32, "concurrent probes")
	flag.IntVar(&o.tryCandidates, "try-candidates", 3, "how many ranked nodes to try before giving up")
	flag.Parse()
	return o
}

func run(ctx context.Context, o options) error {
	if o.subscription == "" {
		return errors.New("-subscription or GITEA_PROXY_AGENT_SUBSCRIPTION is required")
	}
	listenHost, listenPortRaw, err := net.SplitHostPort(o.listen)
	if err != nil {
		return fmt.Errorf("invalid -listen: %w", err)
	}
	if ip := net.ParseIP(listenHost); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("-listen must be a loopback address, got %q", listenHost)
	}
	listenPort, err := strconv.Atoi(listenPortRaw)
	if err != nil || listenPort < 1 || listenPort > 65534 {
		return fmt.Errorf("-listen port must be between 1 and 65534, got %q", listenPortRaw)
	}
	coreAddr := fmt.Sprintf("127.0.0.1:%d", listenPort+1)

	dir, err := os.MkdirTemp("", "gitea-proxy-agent-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	ranked, err := fetchAndRank(ctx, o)
	if err != nil {
		return err
	}

	sess, node, err := bootBest(ctx, o, dir, coreAddr, ranked)
	if err != nil {
		return err
	}
	defer sess.stop()

	rl, err := newRelay(o.listen, coreAddr)
	if err != nil {
		return err
	}
	defer rl.close()

	log.Printf("ready: listen=%s core=%s node=%s", o.listen, coreAddr, node)
	fmt.Printf("ready listen=%s node=%s\n", o.listen, node)

	return supervise(ctx, o, dir, coreAddr, rl, node, sess)
}

// bootBest walks the ranked candidates until one starts and survives the
// verification request.
func bootBest(ctx context.Context, o options, dir, coreAddr string, ranked []Node) (*coreSession, Node, error) {
	limit := o.tryCandidates
	if limit <= 0 || limit > len(ranked) {
		limit = len(ranked)
	}

	var lastErr error
	for i := 0; i < limit; i++ {
		node := ranked[i]
		sess, err := boot(ctx, o, dir, coreAddr, node)
		if err != nil {
			lastErr = err
			log.Printf("candidate %s rejected: %v", node, err)
			continue
		}
		log.Printf("selected node %s (rank %d/%d)", node, i+1, len(ranked))
		return sess, node, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no candidate could be started")
	}
	return nil, Node{}, lastErr
}

func boot(ctx context.Context, o options, dir, coreAddr string, node Node) (*coreSession, error) {
	cfgPath, err := writeConfig(dir, node, coreAddr)
	if err != nil {
		return nil, err
	}
	sess, err := startCore(o.core, cfgPath, coreAddr, o.startTimeout)
	if err != nil {
		return nil, err
	}
	if o.checkURL == "" {
		return sess, nil
	}

	verifyCtx, cancel := context.WithTimeout(ctx, o.verifyTimeout)
	defer cancel()
	if err := verifyThroughProxy(verifyCtx, coreAddr, o.checkURL, o.verifyTimeout); err != nil {
		sess.stop()
		return nil, fmt.Errorf("verification through the node failed: %w", err)
	}
	return sess, nil
}

// supervise keeps the core alive and exits once the proxy has been unused for
// the configured idle period.
func supervise(ctx context.Context, o options, dir, coreAddr string, rl *relay, node Node, sess *coreSession) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("signal received, shutting down")
			return nil

		case <-sess.done:
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			log.Printf("core exited (%v), re-selecting", sess.err)
			ranked, err := fetchAndRank(ctx, o)
			if err != nil {
				return err
			}
			next, nextNode, err := bootBest(ctx, o, dir, coreAddr, ranked)
			if err != nil {
				return err
			}
			sess, node = next, nextNode

		case <-ticker.C:
			if o.idle > 0 && rl.activeConnections() == 0 && rl.idleFor() > o.idle {
				log.Printf("idle for %s, exiting", rl.idleFor().Truncate(time.Second))
				return nil
			}
		}
	}
}

func fetchAndRank(ctx context.Context, o options) ([]Node, error) {
	pickCtx, cancel := context.WithTimeout(ctx, o.pickTimeout)
	defer cancel()

	nodes, err := FetchSubscription(pickCtx, o.subscription, o.subTimeout)
	if err != nil {
		return nil, err
	}
	log.Printf("subscription returned %d nodes", len(nodes))

	ranked, err := RankNodes(pickCtx, nodes, o.probeTimeout, o.maxDelay, o.concurrency)
	if err != nil {
		return nil, err
	}
	log.Printf("%d nodes reachable", len(ranked))
	return ranked, nil
}

func writeConfig(dir string, node Node, coreAddr string) (string, error) {
	raw, err := BuildSingBoxConfig(node, coreAddr)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "sing-box.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

type coreSession struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startCore(bin, cfgPath, addr string, timeout time.Duration) (*coreSession, error) {
	cmd := exec.Command(bin, "run", "-c", cfgPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start core %q: %w", bin, err)
	}

	sess := &coreSession{cmd: cmd, done: make(chan struct{})}
	go func() {
		sess.err = cmd.Wait()
		close(sess.done)
	}()

	if err := waitPort(addr, timeout); err != nil {
		sess.stop()
		return nil, err
	}
	return sess, nil
}

func (s *coreSession) stop() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
	}
}

func waitPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("core did not listen on %s within %s", addr, timeout)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
