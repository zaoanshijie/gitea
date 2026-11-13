// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package launcher

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix is the common prefix of every setting this package reads.
//
// Nothing is read from app.ini on purpose: an instance that does not set these
// variables behaves exactly like the official build, and an instance that sets
// them to something invalid falls back to the official behaviour instead of
// failing to start.
const EnvPrefix = "GITEA_PROXY_AGENT_"

// skippedByDefault keeps the well known domestic forges on the direct path.
// They are reachable without a proxy, so routing them through one only adds
// latency and burns proxy traffic.
var skippedByDefault = []string{"gitee.com", "gitcode.com", "atomgit.com", "gitee.io"}

// Config is the fully validated description of an on-demand proxy.
type Config struct {
	// Command is the absolute path of the executable to spawn on demand.
	Command string
	// Args is the argument list; {addr} and {idle} are substituted.
	Args []string
	// ListenAddr is the loopback address the proxy is expected to serve on.
	ListenAddr string
	// ProxyURL is what git's egress layer is pointed at (a socks5 URL).
	ProxyURL *url.URL
	// SkipHosts are substring patterns that always go direct.
	SkipHosts []string
	// AllowHosts, when non-empty, restricts proxying to these host patterns.
	AllowHosts []string
	// StartTimeout bounds how long a cold start (subscription fetch + node
	// probing + process spawn) may take before we give up and go direct.
	StartTimeout time.Duration
	// ProbeTimeout bounds a single connectivity probe.
	ProbeTimeout time.Duration
	// ProbeTTL is how long a successful probe is trusted for a given host.
	ProbeTTL time.Duration
	// Idle is handed to the agent so it can terminate itself when unused.
	Idle time.Duration
}

// Load reads and validates the configuration from the environment.
//
// The second return value reports whether the feature is enabled. A disabled
// feature is never an error: it means "behave like the official build". An
// error is only returned for a configuration that was asked for but cannot be
// honoured, and callers must treat it as "disabled" too.
func Load() (*Config, bool, error) {
	if !envBool(EnvPrefix + "ENABLED") {
		return nil, false, nil
	}

	cfg := &Config{
		Command:      strings.TrimSpace(os.Getenv(EnvPrefix + "COMMAND")),
		ListenAddr:   fmt.Sprintf("127.0.0.1:%d", envInt(EnvPrefix+"PORT", 17890)),
		StartTimeout: envDuration(EnvPrefix+"START_TIMEOUT", 60*time.Second),
		ProbeTimeout: envDuration(EnvPrefix+"PROBE_TIMEOUT", 5*time.Second),
		ProbeTTL:     envDuration(EnvPrefix+"PROBE_TTL", 60*time.Second),
		Idle:         envDuration(EnvPrefix+"IDLE", 90*time.Second),
		SkipHosts:    envList(EnvPrefix+"SKIP_HOSTS", skippedByDefault),
		AllowHosts:   envList(EnvPrefix+"ALLOW_HOSTS", nil),
	}

	if err := validateCommand(cfg.Command); err != nil {
		return nil, false, err
	}
	if err := validateListenAddr(cfg.ListenAddr); err != nil {
		return nil, false, err
	}
	if cfg.StartTimeout <= 0 || cfg.ProbeTimeout <= 0 || cfg.ProbeTTL < 0 || cfg.Idle < 0 {
		return nil, false, fmt.Errorf("%stimeouts must be positive", EnvPrefix)
	}

	proxyURL, err := url.Parse("socks5://" + cfg.ListenAddr)
	if err != nil {
		return nil, false, fmt.Errorf("invalid listen address %q: %w", cfg.ListenAddr, err)
	}
	cfg.ProxyURL = proxyURL

	cfg.Args = splitArgs(os.Getenv(EnvPrefix + "ARGS"))
	if len(cfg.Args) == 0 {
		// Defaults for the bundled agent; a custom COMMAND needs custom ARGS.
		cfg.Args = []string{"-listen", "{addr}", "-idle", "{idle}"}
	}

	return cfg, true, nil
}

func validateCommand(command string) error {
	if command == "" {
		return fmt.Errorf("%sCOMMAND is required", EnvPrefix)
	}
	if !filepath.IsAbs(command) {
		return fmt.Errorf("%sCOMMAND must be an absolute path, got %q", EnvPrefix, command)
	}
	info, err := os.Stat(command)
	if err != nil {
		return fmt.Errorf("%sCOMMAND %q is not usable: %w", EnvPrefix, command, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%sCOMMAND %q is not a regular file", EnvPrefix, command)
	}
	if !isExecutable(info) {
		return fmt.Errorf("%sCOMMAND %q is not executable", EnvPrefix, command)
	}
	return nil
}

func validateListenAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid %sPORT: %w", EnvPrefix, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%sPORT must bind to a loopback address, got %q", EnvPrefix, host)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%sPORT must be between 1 and 65535, got %q", EnvPrefix, port)
	}
	return nil
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def
	}
	return d
}

func envList(key string, def []string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitArgs(raw string) []string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	return fields
}
