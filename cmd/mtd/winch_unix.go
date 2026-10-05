//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyResize relays terminal resizes (SIGWINCH) to c.
func notifyResize(c chan<- os.Signal) {
	signal.Notify(c, syscall.SIGWINCH)
}
