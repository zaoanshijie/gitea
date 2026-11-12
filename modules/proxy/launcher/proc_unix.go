// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !windows

package launcher

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// isExecutable reports whether the file may be spawned on this platform.
func isExecutable(info os.FileInfo) bool {
	return info.Mode().Perm()&0o111 != 0
}

// setProcAttr puts the agent in its own process group so that the whole tree it
// spawns (the agent plus the proxy core it supervises) is signalled together.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate asks the process group to stop and escalates if it ignores us.
func terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	signal := func(s syscall.Signal) {
		if err == nil {
			_ = syscall.Kill(-pgid, s)
		} else {
			_ = cmd.Process.Signal(s)
		}
	}
	signal(syscall.SIGTERM)
	go func() {
		time.Sleep(3 * time.Second)
		signal(syscall.SIGKILL)
	}()
}
