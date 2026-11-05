//go:build windows

package main

import (
	"os"
	"testing"
)

func TestProcUsageCurrentProcess(t *testing.T) {
	cpu, rss, ok := procUsage(os.Getpid())
	if !ok || cpu < 0 || rss <= 0 {
		t.Fatalf("procUsage(current) = (%v, %d, %v)", cpu, rss, ok)
	}
}

func TestProcUsageInvalidPID(t *testing.T) {
	if _, _, ok := procUsage(-1); ok {
		t.Fatal("procUsage(-1) unexpectedly succeeded")
	}
}
