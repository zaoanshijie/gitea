// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package launcher

import (
	"os"
	"path/filepath"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"ENABLED", "COMMAND", "ARGS", "PORT", "SKIP_HOSTS",
		"ALLOW_HOSTS", "START_TIMEOUT", "PROBE_TIMEOUT", "PROBE_TTL", "IDLE",
	} {
		t.Setenv(EnvPrefix+key, "")
	}
}

func writeFakeBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDisabledWhenUnset(t *testing.T) {
	clearEnv(t)

	cfg, enabled, err := Load()
	if err != nil {
		t.Fatalf("an unset configuration must not be an error, got %v", err)
	}
	if enabled || cfg != nil {
		t.Fatalf("expected the feature to stay disabled, got enabled=%v cfg=%+v", enabled, cfg)
	}
}

func TestLoadRejectsIncompleteConfiguration(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"no command": func(t *testing.T) {},
		"relative command": func(t *testing.T) {
			t.Setenv(EnvPrefix+"COMMAND", "agent")
		},
		"missing binary": func(t *testing.T) {
			t.Setenv(EnvPrefix+"COMMAND", filepath.Join(t.TempDir(), "nope"))
		},
		"bad port": func(t *testing.T) {
			t.Setenv(EnvPrefix+"COMMAND", writeFakeBinary(t))
			t.Setenv(EnvPrefix+"PORT", "99999")
		},
	}

	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(EnvPrefix+"ENABLED", "true")
			setup(t)

			_, enabled, err := Load()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if enabled {
				t.Fatal("an invalid configuration must not be enabled")
			}
		})
	}
}

func TestLoadAcceptsValidConfiguration(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"ENABLED", "1")
	t.Setenv(EnvPrefix+"COMMAND", writeFakeBinary(t))
	t.Setenv(EnvPrefix+"PORT", "17890")
	t.Setenv(EnvPrefix+"SKIP_HOSTS", "gitee.com, gitcode.com")

	cfg, enabled, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Fatal("expected the feature to be enabled")
	}
	if cfg.ListenAddr != "127.0.0.1:17890" {
		t.Errorf("listen addr = %q", cfg.ListenAddr)
	}
	if got := cfg.ProxyURL.String(); got != "socks5://127.0.0.1:17890" {
		t.Errorf("proxy url = %q", got)
	}
	if len(cfg.Args) != 4 || cfg.Args[0] != "-listen" || cfg.Args[3] != "{idle}" {
		t.Errorf("default args = %v", cfg.Args)
	}
	if len(cfg.SkipHosts) != 2 || cfg.SkipHosts[0] != "gitee.com" {
		t.Errorf("skip hosts = %v", cfg.SkipHosts)
	}
	if cfg.Idle <= 0 || cfg.StartTimeout <= 0 {
		t.Errorf("timeouts not defaulted: %+v", cfg)
	}
}

func TestLoadUsesDefaultSkipList(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"ENABLED", "true")
	t.Setenv(EnvPrefix+"COMMAND", writeFakeBinary(t))

	cfg, enabled, err := Load()
	if err != nil || !enabled {
		t.Fatalf("unexpected result: enabled=%v err=%v", enabled, err)
	}
	found := false
	for _, h := range cfg.SkipHosts {
		if h == "gitee.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("default skip list should contain gitee.com, got %v", cfg.SkipHosts)
	}
}
