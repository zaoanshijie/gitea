// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Command fakecore stands in for sing-box during the end-to-end test: it reads
// the generated configuration, binds the SOCKS port it finds there, accepts the
// SOCKS5 handshake and answers every request with 204. It lives under testdata
// so that it is never part of a normal build.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

type config struct {
	Inbounds []struct {
		Listen     string `json:"listen"`
		ListenPort int    `json:"listen_port"`
	} `json:"inbounds"`
}

func main() {
	args := os.Args[1:]
	if len(args) != 3 || args[0] != "run" || args[1] != "-c" {
		fmt.Fprintln(os.Stderr, "fakecore: expected run -c <config>")
		os.Exit(2)
	}

	raw, err := os.ReadFile(args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakecore: %v\n", err)
		os.Exit(2)
	}

	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "fakecore: %v\n", err)
		os.Exit(2)
	}
	if len(cfg.Inbounds) == 0 {
		fmt.Fprintln(os.Stderr, "fakecore: no inbound")
		os.Exit(2)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Inbounds[0].Listen, cfg.Inbounds[0].ListenPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakecore: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "fakecore: listening on %s\n", addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handle(conn)
	}
}

const noContent = "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"

func handle(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 512)

	// greeting: ver, nmethods, methods
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return
	}
	if _, err := io.ReadFull(conn, buf[:int(buf[1])]); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}

	// request: ver, cmd, rsv, atyp, addr, port
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
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	_, _ = conn.Write([]byte(noContent))

	// Drain whatever the client still has in flight. Closing a socket while
	// unread data is pending makes the peer observe a reset instead of a clean
	// end of stream, which shows up as a flaky "connection aborted" on Windows.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.Copy(io.Discard, conn)
}
