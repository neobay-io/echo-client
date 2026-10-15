//go:build !darwin && !linux

package core

import "os/exec"

func prepareBoundedCommand(_ *exec.Cmd) {}
