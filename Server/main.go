//go:build windows

package main

import (
	"akitafrp/internal/ui"
	"os"
	"os/exec"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--server-child" {
		if err := ui.Run("server"); err != nil {
			panic(err)
		}
		return
	}
	// Keep a tiny hidden supervisor process alive so an unexpected process
	// crash (including an unrecovered runtime panic) can restart the GUI.
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	delay := time.Second
	for {
		cmd := exec.Command(exe, "--server-child")
		if err := cmd.Run(); err == nil {
			return // normal user exit
		}
		time.Sleep(delay)
		delay *= 2
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
	}
}
