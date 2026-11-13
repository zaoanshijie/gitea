// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// tempDir is t.TempDir with a tolerant removal: a process that has just been
// killed can keep its executable mapped for a short while on Windows, which
// would otherwise fail the cleanup of an unrelated assertion.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "proxyagent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	return dir
}

func buildBinary(t *testing.T, out, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, combined)
	}
}

func waitForPort(t *testing.T, addr string, timeout time.Duration, output *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if output != nil {
		t.Fatalf("%s never started listening\n%s", addr, output.String())
	}
	t.Fatalf("%s never started listening", addr)
}

// TestAgentEndToEnd wires a subscription served over HTTP, a stand-in node and
// a stand-in core together and checks the whole lifecycle: fetch, rank, spawn,
// verify, forward, idle exit.
func TestAgentEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries")
	}

	dir := tempDir(t)
	fakecore := filepath.Join(dir, exeName("fakecore"))
	agent := filepath.Join(dir, exeName("agent"))
	buildBinary(t, fakecore, "./testdata/fakecore")
	buildBinary(t, agent, ".")

	// The node the agent will pick: any TCP endpoint answers the handshake probe.
	nodeHost, nodePort := hostPortOf(t, echoServer(t))

	cred := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pw"))
	subscription := fmt.Sprintf("ss://%s@%s:%d#local\n", cred, nodeHost, nodePort)

	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, subscription)
	}))
	defer sub.Close()

	listen := freeAddr(t)

	var output bytes.Buffer
	cmd := exec.Command(agent,
		"-listen", listen,
		"-subscription", sub.URL,
		"-core", fakecore,
		"-speed-window", "0",
		"-check-url", "http://verify.invalid/check",
		"-idle", "2s",
		"-probe-timeout", "1s",
		"-pick-timeout", "10s",
		"-start-timeout", "10s",
		"-verify-timeout", "5s",
	)
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	waitForPort(t, listen, 40*time.Second, &output)

	// A real request through the relay must reach the stand-in core.
	if err := verifyThroughProxy(t.Context(), listen, "http://verify.invalid/check", 5*time.Second); err != nil {
		t.Fatalf("request through the agent failed: %v\n%s", err, output.String())
	}

	// The agent must terminate itself once nothing uses it any more.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("agent exited with %v\n%s", err, output.String())
		}
		// Give the OS a moment to release the stand-in core before the temp
		// directory is removed; on Windows an exit handle can lag slightly.
		time.Sleep(500 * time.Millisecond)
	case <-time.After(25 * time.Second):
		t.Fatalf("agent did not exit when idle\n%s", output.String())
	}
}

func TestAgentExitsWhenNoNodeWorks(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}

	dir := tempDir(t)
	agent := filepath.Join(dir, exeName("agent"))
	buildBinary(t, agent, ".")

	deadHost, deadPort := hostPortOf(t, freeAddr(t))
	cred := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pw"))

	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "ss://%s@%s:%d#dead\n", cred, deadHost, deadPort)
	}))
	defer sub.Close()

	listen := freeAddr(t)
	cmd := exec.Command(agent,
		"-listen", listen,
		"-subscription", sub.URL,
		"-core", filepath.Join(dir, "unused"),
		"-probe-timeout", "500ms",
		"-pick-timeout", "5s",
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the agent must fail when no node is reachable")
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the agent hung instead of failing")
	}
}
