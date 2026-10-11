//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"runtime"
)

func openVirtualPads(count int, kind string) ([]padDevice, func(), error) {
	return nil, nil, errors.New("virtual controllers need Windows")
}

func openBrowser(url string) {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", url).Start()
	} else {
		_ = exec.Command("xdg-open", url).Start()
	}
}

func hideWindow(cmd *exec.Cmd) {}

func ikemenRunning() bool { return true }
