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
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type options struct {
	listen       string
	subscription string
	core         string
	data         string
	checkURL     string
	speedURL     string
	target       string
	speedWindow  time.Duration
	speedConns   int
	minSpeed     int64

	idle          time.Duration
	maxDelay      time.Duration
	probeTimeout  time.Duration
	subTimeout    time.Duration
	startTimeout  time.Duration
	startBudget   time.Duration
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
	flag.StringVar(&o.core, "core", strings.TrimSpace(os.Getenv(EnvCore)), "path of the sing-box binary (default: <base>/sing-box/sing-box)")
	flag.StringVar(&o.data, "data", strings.TrimSpace(os.Getenv(EnvData)), "directory holding the core state (default: <base>/sing-box)")
	flag.StringVar(&o.checkURL, "check-url", "https://github.com/robots.txt", "URL used to verify the chosen node, empty disables the check")
	flag.DurationVar(&o.idle, "idle", 90*time.Second, "exit after this long without any connection (0 disables)")
	flag.DurationVar(&o.maxDelay, "max-delay", 5*time.Second, "drop nodes slower than this handshake")
	flag.DurationVar(&o.probeTimeout, "probe-timeout", 3*time.Second, "per node handshake timeout")
	flag.DurationVar(&o.subTimeout, "subscription-timeout", 20*time.Second, "subscription fetch timeout")
	flag.DurationVar(&o.startTimeout, "start-timeout", 15*time.Second, "how long sing-box may take to bind")
	flag.DurationVar(&o.startBudget, "start-budget", 30*time.Second, "hard cap for fetch + rank + node selection, so the caller stops waiting in bounded time")
	flag.DurationVar(&o.pickTimeout, "pick-timeout", 20*time.Second, "budget for the subscription fetch and the latency ranking")
	flag.DurationVar(&o.verifyTimeout, "verify-timeout", 12*time.Second, "how long the verification request may take")
	flag.StringVar(&o.target, "target", envTarget(), "host the proxy is started for; throughput is measured against it")
	flag.StringVar(&o.speedURL, "speed-url", "", "explicit URL used to measure throughput, overrides -target")
	flag.DurationVar(&o.speedWindow, "speed-window", 3*time.Second, "how long to sample throughput per candidate")
	flag.IntVar(&o.speedConns, "speed-conns", 4, "parallel connections used by the throughput probe")
	flag.Int64Var(&o.minSpeed, "min-speed", 80*1024, "accept the first candidate at least this fast (bytes/s), 0 disables the check")
	flag.IntVar(&o.concurrency, "concurrency", 32, "concurrent probes")
	flag.IntVar(&o.tryCandidates, "try-candidates", 4, "how many ranked nodes to try before giving up")
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

	layout, err := resolvePaths(o.core, o.data, log.Printf)
	if err != nil {
		return err
	}
	defer layout.cleanUp()
	o.core = layout.core

	if closer := redirectOutput(layout.logFile()); closer != nil {
		defer closer()
	}
	log.Printf("base dir %s, data dir %s, core %s", layout.base, layout.dataDir, layout.core)
	if u := o.probeURL(); u != "" {
		log.Printf("throughput target %s", u)
	}

	// The launcher waits for the SOCKS port before handing git over, so the
	// whole cold start has to finish in bounded time even when several
	// candidates have to be measured.
	startCtx, cancelStart := context.WithTimeout(ctx, o.startBudget)
	defer cancelStart()

	ranked, err := fetchAndRank(startCtx, o)
	if err != nil {
		return err
	}

	sess, node, err := bootBest(startCtx, o, layout, coreAddr, ranked)
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

	return supervise(ctx, o, layout, coreAddr, rl, node, sess)
}

// defaultSpeedURL is only used when the launcher did not say which host the
// proxy is for. Any HTTPS endpoint would do in that case.
const defaultSpeedURL = "https://github.com/robots.txt"

// probeURL picks what to measure: an explicit -speed-url wins, otherwise the
// host the proxy was started for. Measuring the real target is the point --
// a node can be fast to Cloudflare and crawl towards github.com.
func (o options) probeURL() string {
	urls := o.probeURLs()
	if len(urls) == 0 {
		return ""
	}
	return urls[0]
}

// probeURLs returns the URLs to measure, in order of preference.
//
// The target may be a bare host, a host:port or a full remote URL. For a
// repository URL the smart-HTTP refs endpoint is preferred: that is exactly the
// request git makes, and its response is large enough to time. A bare host is
// probed at its root, falling back to plain HTTP because git servers often
// answer HTTPS with a redirect and no body.
func (o options) probeURLs() []string {
	if o.speedURL != "" {
		return []string{o.speedURL}
	}
	target := strings.TrimSpace(o.target)
	if target == "" {
		return []string{defaultSpeedURL}
	}

	var urls []string
	if strings.Contains(target, "://") {
		repo := strings.TrimSuffix(target, "/")
		if strings.HasSuffix(repo, ".git") {
			urls = append(urls, repo+"/info/refs?service=git-upload-pack")
		}
		urls = append(urls, repo+"/")
		if u, err := url.Parse(repo); err == nil && u.Host != "" {
			urls = append(urls, u.Scheme+"://"+u.Host+"/")
		}
	} else {
		host := target
		if h, p, err := net.SplitHostPort(target); err == nil {
			host = h
			if p != "" && p != "443" {
				host = net.JoinHostPort(h, p)
			}
		}
		urls = append(urls, "https://"+host+"/", "http://"+host+"/")
	}
	return dedupeStrings(urls)
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func envTarget() string { return strings.TrimSpace(os.Getenv(EnvTarget)) }

// bootBest walks the ranked candidates and keeps the fastest one that starts,
// answers a real request and moves bytes at an acceptable rate.
//
// Ranking alone is not enough: the endpoint with the lowest handshake latency
// is usually the most oversubscribed, which is exactly the wrong choice for a
// mirror clone of a few hundred megabytes.
func bootBest(ctx context.Context, o options, layout paths, coreAddr string, ranked []Node) (*coreSession, Node, error) {
	limit := o.tryCandidates
	if limit <= 0 || limit > len(ranked) {
		limit = len(ranked)
	}

	var (
		best     *coreSession
		bestNode Node
		bestBps  int64 = -1
		lastErr  error

		probeURLs = o.probeURLs()
		pinnedURL string
	)

	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			log.Printf("selection budget reached after %d candidates", i)
			break
		}
		node := ranked[i]
		sess, err := boot(ctx, o, layout, coreAddr, node)
		if err != nil {
			lastErr = err
			log.Printf("candidate %s rejected: %v", node, err)
			continue
		}

		bps := int64(0)
		if o.speedWindow > 0 && len(probeURLs) > 0 {
			try := probeURLs
			if pinnedURL != "" {
				try = []string{pinnedURL}
			}
			probeCtx, cancel := context.WithTimeout(ctx, time.Duration(len(try))*o.speedWindow+4*time.Second)
			var used string
			bps, used, err = probeAny(probeCtx, coreAddr, try, o.speedWindow, o.speedConns, 8<<20)
			cancel()
			switch {
			case err != nil:
				// The host may refuse plain HTTP requests while still serving
				// git, so keep the candidate and mark its speed unknown.
				log.Printf("candidate %s (rank %d/%d): speed unknown (%v)", node, i+1, len(ranked), err)
				bps = 0
			default:
				pinnedURL = used
				log.Printf("candidate %s (rank %d/%d): %d KB/s via %s", node, i+1, len(ranked), bps/1024, used)
			}
		}

		if bps > bestBps {
			if best != nil {
				best.stop()
			}
			best, bestNode, bestBps = sess, node, bps
		} else {
			sess.stop()
		}

		if o.minSpeed > 0 && bestBps >= o.minSpeed {
			break
		}
		if ctx.Err() != nil {
			// Out of budget: keep what was measured and stop booting cores.
			log.Printf("selection budget reached after %d candidates", i+1)
			break
		}
	}

	if best == nil {
		if lastErr == nil {
			lastErr = errors.New("no candidate could be started")
		}
		return nil, Node{}, lastErr
	}
	if o.minSpeed > 0 && bestBps >= 0 && bestBps < o.minSpeed {
		log.Printf("no candidate reached %d KB/s, using the fastest anyway: %s at %d KB/s",
			o.minSpeed/1024, bestNode, bestBps/1024)
	}
	log.Printf("selected node %s (%d KB/s)", bestNode, bestBps/1024)
	return best, bestNode, nil
}

func boot(ctx context.Context, o options, layout paths, coreAddr string, node Node) (*coreSession, error) {
	cfgPath, err := writeConfig(layout, node, coreAddr)
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
func supervise(ctx context.Context, o options, layout paths, coreAddr string, rl *relay, node Node, sess *coreSession) error {
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
			reselectCtx, cancel := context.WithTimeout(ctx, o.startBudget)
			ranked, err := fetchAndRank(reselectCtx, o)
			if err != nil {
				cancel()
				return err
			}
			next, nextNode, err := bootBest(reselectCtx, o, layout, coreAddr, ranked)
			cancel()
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

func writeConfig(layout paths, node Node, coreAddr string) (string, error) {
	raw, err := BuildSingBoxConfig(node, coreAddr, layout.cacheFile())
	if err != nil {
		return "", err
	}
	path := layout.configFile()
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
	cmd.Stdout = coreOutput
	cmd.Stderr = coreOutput
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
