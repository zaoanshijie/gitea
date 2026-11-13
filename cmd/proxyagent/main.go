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
	"sync/atomic"
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
	requireSpeed bool

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
	flag.StringVar(&o.checkURL, "check-url", "https://github.com/robots.txt", "fallback URL used to verify a node when the target itself cannot be probed, empty disables it")
	flag.DurationVar(&o.idle, "idle", 90*time.Second, "exit after this long without any connection (0 disables)")
	flag.DurationVar(&o.maxDelay, "max-delay", 5*time.Second, "drop nodes slower than this handshake")
	flag.DurationVar(&o.probeTimeout, "probe-timeout", 3*time.Second, "per node handshake timeout")
	flag.DurationVar(&o.subTimeout, "subscription-timeout", 20*time.Second, "subscription fetch timeout")
	flag.DurationVar(&o.startTimeout, "start-timeout", 15*time.Second, "how long sing-box may take to bind")
	flag.DurationVar(&o.startBudget, "start-budget", 40*time.Second, "hard cap for fetch + rank + node selection, so the caller stops waiting in bounded time")
	flag.DurationVar(&o.pickTimeout, "pick-timeout", 20*time.Second, "budget for the subscription fetch and the latency ranking")
	flag.DurationVar(&o.verifyTimeout, "verify-timeout", 8*time.Second, "how long the verification request may take")
	flag.StringVar(&o.target, "target", envTarget(), "host the proxy is started for; throughput is measured against it")
	flag.StringVar(&o.speedURL, "speed-url", "", "explicit URL used to measure throughput, overrides -target")
	flag.DurationVar(&o.speedWindow, "speed-window", 2*time.Second, "how long to sample throughput per candidate")
	flag.IntVar(&o.speedConns, "speed-conns", 4, "parallel connections used by the throughput probe")
	flag.Int64Var(&o.minSpeed, "min-speed", 80*1024, "accept the first candidate at least this fast (bytes/s), 0 disables the check")
	flag.BoolVar(&o.requireSpeed, "require-speed", true, "refuse to serve a node that moved no data towards the target: direct is better than a dead tunnel")
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
	// Cores that were killed by an earlier run leave their per-instance files
	// behind; they are never reused, so drop the old ones.
	layout.removeStale(2 * time.Hour)
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

// bootBest walks the ranked candidates and keeps the fastest one that starts
// and answers a real request towards the target.
//
// Ranking alone is not enough: the endpoint with the lowest handshake latency
// is usually the most oversubscribed, which is exactly the wrong choice for a
// mirror clone of a few hundred megabytes.
//
// The throughput probe doubles as the healthcheck. Verifying against a fixed
// URL would be wrong: a node may reach one host and not another, and the whole
// point of the target-aware selection is that the two differ.
func bootBest(ctx context.Context, o options, layout paths, coreAddr string, ranked []Node) (*coreSession, Node, error) {
	limit := o.tryCandidates
	if limit <= 0 || limit > len(ranked) {
		limit = len(ranked)
	}

	var (
		best     *coreSession
		bestNode Node
		bestBps  int64 = -1 // -1: no measurement yet
		haveBps  bool
		tried    int
		lastErr  error

		// A node that is reachable but could not be measured is remembered by
		// value only. Keeping its core alive would hold the core port and make
		// every later candidate fail to bind.
		parkedNode *Node

		probeURLs = o.probeURLs()
		pinnedURL string
	)

	// A node that answers the healthcheck but moves no bytes is not usable for
	// a clone, and picking it is what produced the 502/504 storm: the mirror
	// job then fails anyway, just later and more expensively.
	requireSpeed := o.requireSpeed && len(probeURLs) > 0 && o.speedWindow > 0

	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			log.Printf("selection budget reached after %d candidates", i)
			break
		}
		node := ranked[i]
		tried++
		sess, err := boot(ctx, o, layout, coreAddr, node)
		if err != nil {
			lastErr = err
			log.Printf("candidate %s rejected: %v", node, err)
			continue
		}

		bps := int64(0)
		measured := false
		if len(probeURLs) > 0 && o.speedWindow > 0 {
			bps, err = measure(ctx, o, coreAddr, probeURLs, &pinnedURL)
			measured = err == nil
		}
		if !measured {
			// Either speed measurement is off, or the target refused a plain
			// request. Try the cheap healthcheck so a working node is not
			// discarded, and accept the node when nothing can be checked.
			if o.checkURL != "" && ctx.Err() == nil {
				verifyCtx, cancel := context.WithTimeout(ctx, o.verifyTimeout)
				err = verifyThroughProxy(verifyCtx, coreAddr, o.checkURL, o.verifyTimeout)
				cancel()
			} else if o.checkURL == "" {
				err = nil
			}
			if err != nil {
				lastErr = err
				log.Printf("candidate %s (rank %d/%d) rejected: %v", node, i+1, len(ranked), err)
				sess.stop()
				continue
			}
			if len(probeURLs) > 0 && o.speedWindow > 0 {
				log.Printf("candidate %s (rank %d/%d): reachable, throughput unknown", node, i+1, len(ranked))
			}
			bps = 0
		}

		// A measured candidate always wins and the fastest measurement wins.
		// Everything else gives its core back: only one core may exist at a
		// time, because they all bind the same port.
		if measured && (!haveBps || bps > bestBps) {
			if best != nil {
				best.stop()
			}
			best, bestNode, bestBps, haveBps = sess, node, bps, true
		} else {
			if !measured && parkedNode == nil {
				parked := node
				parkedNode = &parked
			}
			sess.stop()
		}

		if o.minSpeed > 0 && haveBps && bestBps >= o.minSpeed {
			break
		}
		if ctx.Err() != nil {
			log.Printf("selection budget reached after %d candidates", i+1)
			break
		}
	}

	if best == nil {
		if requireSpeed && tried > 0 {
			msg := fmt.Sprintf("no candidate moved any data from %s after %d candidate(s)", probeURLs[0], tried)
			if lastErr != nil {
				return nil, Node{}, fmt.Errorf("%s: %w", msg, lastErr)
			}
			return nil, Node{}, errors.New(msg)
		}
		if parkedNode != nil {
			// Measurement was not conclusive but the node did answer; start it
			// once more now that the port is free.
			sess, err := boot(ctx, o, layout, coreAddr, *parkedNode)
			if err != nil {
				return nil, Node{}, err
			}
			log.Printf("using reachable node %s without a throughput measurement", *parkedNode)
			return sess, *parkedNode, nil
		}
		if lastErr == nil {
			lastErr = errors.New("no candidate could be started")
		}
		return nil, Node{}, lastErr
	}
	if o.minSpeed > 0 && haveBps && bestBps >= 0 && bestBps < o.minSpeed {
		log.Printf("no candidate reached %d KB/s, using the fastest anyway: %s at %d KB/s",
			o.minSpeed/1024, bestNode, bestBps/1024)
	}
	log.Printf("selected node %s (%d KB/s)", bestNode, bestBps/1024)
	return best, bestNode, nil
}

// measure samples the target through the node, reusing the address that worked
// for the previous candidate so later ones are not slowed down by re-probing
// schemes that are known to yield nothing.
func measure(ctx context.Context, o options, coreAddr string, probeURLs []string, pinned *string) (int64, error) {
	if o.speedWindow <= 0 || len(probeURLs) == 0 {
		return 0, nil
	}
	try := probeURLs
	if *pinned != "" {
		try = []string{*pinned}
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(len(try))*o.speedWindow+6*time.Second)
	defer cancel()

	bps, used, err := probeAny(probeCtx, coreAddr, try, o.speedWindow, o.speedConns, 8<<20)
	if err != nil {
		return 0, err
	}
	*pinned = used
	log.Printf("throughput %d KB/s via %s", bps/1024, used)
	return bps, nil
}

// boot starts the core for one candidate. Both the spawn and the readiness
// wait are bounded by the caller's budget so a stuck core cannot eat the whole
// selection window.
func boot(ctx context.Context, o options, layout paths, coreAddr string, node Node) (*coreSession, error) {
	cfgPath, cachePath, err := writeConfig(layout, node, coreAddr, nextCoreTag())
	if err != nil {
		return nil, err
	}
	timeout := o.startTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		removeInstanceFiles(cfgPath, cachePath)
		return nil, errors.New("no budget left to start a core")
	}
	return startCore(ctx, o.core, cfgPath, cachePath, coreAddr, timeout)
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

func writeConfig(layout paths, node Node, coreAddr, tag string) (cfgPath, cachePath string, err error) {
	cachePath = layout.instanceCacheFile(tag)
	raw, err := BuildSingBoxConfig(node, coreAddr, cachePath)
	if err != nil {
		return "", "", err
	}
	cfgPath = layout.instanceConfigFile(tag)
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		return "", "", err
	}
	return cfgPath, cachePath, nil
}

type coreSession struct {
	cmd       *exec.Cmd
	done      chan struct{}
	err       error
	cfgPath   string
	cachePath string
}

// coreSeq numbers the core instances of this process so each one gets its own
// config and cache file.
var coreSeq atomic.Int64

func nextCoreTag() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), coreSeq.Add(1))
}

func startCore(ctx context.Context, bin, cfgPath, cachePath, addr string, timeout time.Duration) (*coreSession, error) {
	// A predecessor may still be releasing the port. Waiting here turns a core
	// that would die with "address already in use" into a clean start.
	if err := waitPortFree(ctx, addr, 2*time.Second); err != nil {
		removeInstanceFiles(cfgPath, cachePath)
		return nil, err
	}

	cmd := exec.Command(bin, "run", "-c", cfgPath)
	cmd.Stdout = coreOutput
	cmd.Stderr = coreOutput
	if err := cmd.Start(); err != nil {
		removeInstanceFiles(cfgPath, cachePath)
		return nil, fmt.Errorf("start core %q: %w", bin, err)
	}

	sess := &coreSession{cmd: cmd, done: make(chan struct{}), cfgPath: cfgPath, cachePath: cachePath}
	go func() {
		sess.err = cmd.Wait()
		close(sess.done)
	}()

	if err := waitPort(ctx, addr, timeout); err != nil {
		sess.stop()
		return nil, err
	}
	return sess, nil
}

func (s *coreSession) stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		select {
		case <-s.done:
		case <-time.After(3 * time.Second):
		}
	}
	removeInstanceFiles(s.cfgPath, s.cachePath)
	s.cmd, s.cfgPath, s.cachePath = nil, "", ""
}

func removeInstanceFiles(paths ...string) {
	for _, p := range paths {
		if p != "" {
			_ = os.Remove(p)
		}
	}
}

// waitPortFree blocks until nothing listens on addr, so a core that is still
// being torn down cannot make the next one fail at bind time.
func waitPortFree(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return nil
		}
		_ = conn.Close()
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("%s is still held by another process", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitPort(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("core did not listen on %s: %w", addr, err)
		}
		if conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("core did not listen on %s within %s", addr, timeout)
}
