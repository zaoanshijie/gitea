// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"log"
	"os"
)

// coreOutput receives whatever the core prints. Once a log file is open it
// points at both the file and stderr, so the Gitea launcher keeps forwarding
// the lines into its own log while the data directory keeps a copy.
var coreOutput io.Writer = os.Stderr

// maxLogSize caps the agent log; a mirror run can be noisy and this file lives
// next to the binary, so it must not grow without bound.
const maxLogSize = 4 << 20

// redirectOutput mirrors the agent log into the data directory.
func redirectOutput(path string) func() {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("cannot open %s: %v", path, err)
		return nil
	}
	if info, err := f.Stat(); err == nil && info.Size() > maxLogSize {
		if err := f.Truncate(0); err != nil {
			log.Printf("cannot rotate %s: %v", path, err)
		}
	}

	mw := io.MultiWriter(os.Stderr, f)
	log.SetOutput(mw)
	coreOutput = mw
	return func() { _ = f.Close() }
}
