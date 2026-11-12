// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build windows

package launcher

import (
	"os"
	"os/exec"
)

// isExecutable always reports true on Windows, where the mode carries no
// execute bit.
func isExecutable(os.FileInfo) bool { return true }

func setProcAttr(cmd *exec.Cmd) {}

func terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
