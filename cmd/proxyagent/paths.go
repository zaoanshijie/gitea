// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// EnvBaseDir is exported by the Gitea launcher so the agent knows where the
// Gitea binary lives. Thanks to it a release archive can be unpacked into one
// directory and run without configuring any path at all.
const EnvBaseDir = "GITEA_PROXY_AGENT_BASE_DIR"

// EnvCore keeps the historical way of naming the core binary explicitly.
const EnvCore = "GITEA_PROXY_AGENT_CORE"

// EnvData overrides the data directory. The default next to the binary is what
// makes a release archive self-contained, so this is only needed to keep state
// somewhere else, for instance when the binary lives in a read-only location.
const EnvData = "GITEA_PROXY_AGENT_DATA"

// EnvTarget tells the agent which host it is being started for, so throughput
// can be measured against that host instead of a generic one.
const EnvTarget = "GITEA_PROXY_AGENT_TARGET"

// dataDirName is the directory next to the Gitea binary that holds the core
// binary and everything it writes.
const dataDirName = "sing-box"

// paths is the resolved layout:
//
//	<base>/sing-box/sing-box                    core binary (unless overridden)
//	<base>/sing-box/agent-sing-box-<id>.json    per-instance core config
//	<base>/sing-box/cache-<id>.db               per-instance core cache
//	<base>/sing-box/agent.log                   agent log
type paths struct {
	base      string
	dataDir   string
	core      string
	ephemeral bool
}

func (p paths) cleanUp() {
	if p.ephemeral {
		_ = os.RemoveAll(p.dataDir)
	}
}

func (p paths) cacheFile() string { return filepath.Join(p.dataDir, "cache.db") }
func (p paths) configFile() string {
	return filepath.Join(p.dataDir, "agent-sing-box.json")
}
func (p paths) logFile() string { return filepath.Join(p.dataDir, "agent.log") }

// instanceConfigFile returns the core configuration of one core instance.
//
// Every instance gets its own file: the agent writes the config right before
// spawning a core, and a second agent doing the same would otherwise overwrite
// the node list of the core that is already running.
func (p paths) instanceConfigFile(tag string) string {
	return filepath.Join(p.dataDir, "agent-sing-box-"+tag+".json")
}

// instanceCacheFile returns the cache path of one core instance.
//
// sing-box takes an exclusive lock on its cache file, so a single shared
// cache.db turns any overlap between two cores into a hard start failure
// ("initialize cache-file: timeout"). That is not hypothetical: it happened
// when a retarget overlapped the shutdown of the previous agent, and the cores
// that failed to start are the ones that then reported "speed unknown". The
// cache only holds DNS answers and rule sets, so scoping it to the instance
// costs nothing.
func (p paths) instanceCacheFile(tag string) string {
	return filepath.Join(p.dataDir, "cache-"+tag+".db")
}

// removeStale drops the config and cache files of instances that are long
// gone, so repeated starts do not accumulate files next to the binary.
func (p paths) removeStale(olderThan time.Duration) {
	entries, err := os.ReadDir(p.dataDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-olderThan)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "cache-") && !strings.HasPrefix(name, "agent-sing-box-") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(p.dataDir, name))
	}
}

// resolvePaths works out where the core lives and where it may write.
//
// The data directory is the only thing that can degrade: if it cannot be
// created or written to, a temporary directory stands in and the proxy still
// works, it just forgets its cache. A missing core is a hard error, because
// without it there is nothing to run.
func resolvePaths(coreFlag, dataFlag string, warnf func(string, ...any)) (paths, error) {
	if warnf == nil {
		warnf = func(string, ...any) {}
	}

	p := paths{base: baseDir()}
	intended := filepath.Join(p.base, dataDirName)

	p.dataDir, p.ephemeral = writableDir(dataFlag, intended, warnf)
	p.core = resolveCore(coreFlag, intended, p.dataDir)
	if p.core == "" {
		p.cleanUp()
		return paths{}, fmt.Errorf("no sing-box binary found: expected %s, or set -core / %s",
			filepath.Join(intended, coreName()), EnvCore)
	}
	return p, nil
}

// baseDir prefers what the launcher told us, then the agent's own location.
func baseDir() string {
	if v := strings.TrimSpace(os.Getenv(EnvBaseDir)); v != "" {
		return filepath.Clean(v)
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(realPath(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// writableDir returns the directory to keep state in, falling back to a
// temporary one when the intended directory is unusable.
func writableDir(explicit, intended string, warnf func(string, ...any)) (string, bool) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else {
		candidates = append(candidates, intended)
	}

	for _, dir := range candidates {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			warnf("data directory %q is not usable: %v", dir, err)
			continue
		}
		if err := probeWritable(dir); err != nil {
			warnf("data directory %q is not writable: %v", dir, err)
			continue
		}
		return dir, false
	}

	tmp, err := os.MkdirTemp("", "gitea-proxy-agent-")
	if err != nil {
		// Everything is unwritable; run from the intended path anyway, the
		// core will report the real problem itself.
		warnf("no writable directory, using %q", intended)
		return intended, false
	}
	warnf("state kept in %q, it will be discarded when the agent exits", tmp)
	return tmp, true
}

func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// resolveCore honours an explicit setting first, then the bundled layout, then
// PATH. An explicit path that does not exist is an error: silently falling back
// would hide a typo.
func resolveCore(explicit, intended, dataDir string) string {
	if explicit != "" {
		if isExecutableFile(explicit) {
			return explicit
		}
		return ""
	}

	candidates := []string{
		filepath.Join(intended, coreName()),
		filepath.Join(dataDir, coreName()),
	}
	if env := strings.TrimSpace(os.Getenv(EnvCore)); env != "" {
		candidates = append([]string{env}, candidates...)
	}

	for _, c := range candidates {
		if isExecutableFile(c) {
			return c
		}
	}
	if found, err := exec.LookPath("sing-box"); err == nil {
		return found
	}
	return ""
}

func coreName() string {
	if runtime.GOOS == "windows" {
		return "sing-box.exe"
	}
	return "sing-box"
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}
