// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolvePathsUsesDirectoryNextToTheBinary(t *testing.T) {
	base := t.TempDir()
	t.Setenv(EnvBaseDir, base)
	t.Setenv(EnvCore, "")
	core := writeExecutable(t, filepath.Join(base, dataDirName, coreName()))

	got, err := resolvePaths("", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer got.cleanUp()

	if got.core != core {
		t.Errorf("core = %q, want %q", got.core, core)
	}
	if want := filepath.Join(base, dataDirName); got.dataDir != want {
		t.Errorf("data dir = %q, want %q", got.dataDir, want)
	}
	if got.ephemeral {
		t.Error("data dir must not be ephemeral when it is writable")
	}
	for _, name := range []string{got.configFile(), got.cacheFile(), got.logFile()} {
		if !strings.HasPrefix(name, got.dataDir) {
			t.Errorf("%q escapes the data directory %q", name, got.dataDir)
		}
	}
}

func TestResolvePathsCreatesTheDataDirectory(t *testing.T) {
	base := t.TempDir()
	t.Setenv(EnvBaseDir, base)
	t.Setenv(EnvCore, "")
	writeExecutable(t, filepath.Join(base, dataDirName, coreName()))

	got, err := resolvePaths("", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer got.cleanUp()

	if info, err := os.Stat(got.dataDir); err != nil || !info.IsDir() {
		t.Fatalf("data dir was not created: %v", err)
	}
}

func TestResolvePathsHonoursExplicitSettings(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(t.TempDir(), "nested", "state")
	t.Setenv(EnvBaseDir, base)
	t.Setenv(EnvCore, "")
	core := writeExecutable(t, filepath.Join(base, dataDirName, coreName()))

	got, err := resolvePaths(core, data, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer got.cleanUp()

	if got.core != core {
		t.Errorf("core = %q, want %q", got.core, core)
	}
	if got.dataDir != data {
		t.Errorf("data dir = %q, want %q", got.dataDir, data)
	}
}

func TestResolveCoreFromEnvironment(t *testing.T) {
	base := t.TempDir()
	t.Setenv(EnvBaseDir, base)
	other := writeExecutable(t, filepath.Join(t.TempDir(), "custom-"+coreName()))
	t.Setenv(EnvCore, other)

	got, err := resolvePaths("", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer got.cleanUp()

	if got.core != other {
		t.Errorf("core = %q, want %q", got.core, other)
	}
}

func TestResolvePathsFailsWithoutCore(t *testing.T) {
	base := t.TempDir()
	t.Setenv(EnvBaseDir, base)
	t.Setenv(EnvCore, "")
	// Keep the lookup from finding a real sing-box on PATH.
	t.Setenv("PATH", t.TempDir())

	_, err := resolvePaths("", "", nil)
	if err == nil {
		t.Fatal("expected an error when no core binary exists")
	}
	if !strings.Contains(err.Error(), dataDirName) {
		t.Errorf("error %q should point at the expected location", err)
	}
}

func TestResolvePathsRejectsABrokenExplicitCore(t *testing.T) {
	t.Setenv(EnvBaseDir, t.TempDir())
	t.Setenv(EnvCore, "")

	_, err := resolvePaths(filepath.Join(t.TempDir(), "missing"), "", nil)
	if err == nil {
		t.Fatal("an explicit but missing core must not fall back silently")
	}
}

func TestResolvePathsKeepsWorkingWhenTheDirectoryIsUnusable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced this way on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}

	base := t.TempDir()
	t.Setenv(EnvBaseDir, base)
	t.Setenv(EnvCore, "")
	core := writeExecutable(t, filepath.Join(base, dataDirName, coreName()))

	// A read-only base directory: the core is still found, state moves away.
	if err := os.Chmod(base, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o750) })

	var warnings []string
	got, err := resolvePaths("", "", func(format string, args ...any) {
		warnings = append(warnings, format)
	})
	if err != nil {
		t.Fatalf("an unusable data directory must not be fatal: %v", err)
	}
	defer got.cleanUp()

	if got.core != core {
		t.Errorf("core = %q, want %q", got.core, core)
	}
	if !got.ephemeral {
		t.Error("expected an ephemeral data directory")
	}
	if len(warnings) == 0 {
		t.Error("expected a warning about the fallback")
	}
}

func TestProbeSpeedReportsDeadProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	if _, err := probeSpeed(ctx, "127.0.0.1:1", "https://example.invalid/x", 300*time.Millisecond, 2, 1<<20); err == nil {
		t.Fatal("expected an error when the proxy is not listening")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("probe took %s, it should fail fast", elapsed)
	}
}
