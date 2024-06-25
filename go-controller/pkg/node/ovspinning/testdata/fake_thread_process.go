//go:build testing_ovspinning
// +build testing_ovspinning

package main

// This file is meant to be used in unit tests to validate the behavior of the ovspinnging package.
// The purpose is to simulate a process with multiple threads, like the ovs-vswitchd daemon and Go programs
// spawns a pool of thread by default

import (
	"time"
)

func main() {
	time.Sleep(100 * time.Second)
}
