// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package stop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStopHelper(t *testing.T) {
	file := os.Getenv("SEMANTIC_STOP_READY")
	if file == "" {
		return
	}
	ctx, cancel, err := NotifyContext(context.Background())
	if err != nil {
		panic(err)
	}
	defer cancel()
	if err = os.WriteFile(file, []byte("ready"), 0600); err != nil {
		panic(err)
	}
	<-ctx.Done()
	os.Exit(0)
}
func TestGracefulRequestAndCreationIdentity(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestStopHelper$")
	command.Env = append(os.Environ(), "SEMANTIC_STOP_READY="+file)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(file); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop listener not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	identity, err := Identity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if err = Request(command.Process.Pid, "wrong-creation-time"); err == nil {
			t.Fatal("stale identity accepted")
		}
		if err = Request(command.Process.Pid, ""); err == nil {
			t.Fatal("missing identity accepted")
		}
		select {
		case <-done:
			t.Fatal("invalid request terminated process")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err = Request(command.Process.Pid, identity); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("graceful request did not complete")
	}
}
func TestParentCancellationAndRepeatedClose(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel, err := NotifyContext(parent)
	if err != nil {
		t.Fatal(err)
	}
	cancelParent()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("parent cancellation lost")
	}
	cancel()
	cancel()
}
