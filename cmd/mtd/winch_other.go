//go:build !unix

package main

import "os"

// notifyResize does nothing where there is no SIGWINCH; the renderer
// notices size changes when it next draws instead.
func notifyResize(c chan<- os.Signal) {}
