// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0

package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	stopport "insightos.cn/semantic-framework/internal/ports/stop"
)

func TestProcessHelper(t *testing.T) {
	mode := os.Getenv("SEMANTIC_PROCESS_HELPER")
	if mode == "" {
		return
	}
	ctx, cancel, err := stopport.NotifyContext(context.Background())
	if err != nil {
		panic(err)
	}
	defer cancel()
	var child *exec.Cmd
	if mode == "parent" {
		child = exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
		child.Env = append(os.Environ(), "SEMANTIC_PROCESS_HELPER=child")
		if err = child.Start(); err != nil {
			panic(err)
		}
	}
	directory := os.Getenv("SEMANTIC_PROCESS_DIRECTORY")
	if err = os.WriteFile(filepath.Join(directory, mode+".pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		panic(err)
	}
	<-ctx.Done()
	if child != nil {
		_ = child.Wait()
	}
	os.Exit(0)
}

func startHelper(t *testing.T, mode string) (*exec.Cmd, *Tree, string, <-chan error) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "中文 path with spaces")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
	command.Env = append(os.Environ(), "SEMANTIC_PROCESS_HELPER="+mode, "SEMANTIC_PROCESS_DIRECTORY="+directory)
	tree, err := Start(command)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() { _ = tree.Kill(); _ = tree.Close(); _ = command.Process.Kill() })
	waitFile(t, filepath.Join(directory, mode+".pid"))
	return command, tree, directory, done
}
func waitFile(t *testing.T, path string) []byte {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return data
		}
	}
	t.Fatalf("child did not become ready: %s", path)
	return nil
}
func waitTree(t *testing.T, tree *Tree) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		alive, err := tree.Alive()
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			return
		}
	}
	t.Fatal("owned process tree still active")
}
func TestOwnedDescendantsAndUnrelatedProcess(t *testing.T) {
	_, tree, directory, done := startHelper(t, "parent")
	waitFile(t, filepath.Join(directory, "child.pid"))
	unrelated, _, _, _ := startHelper(t, "unrelated")
	if err := tree.Terminate(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parent did not exit")
	}
	waitTree(t, tree)
	if !Alive(unrelated.Process.Pid) {
		t.Fatal("unrelated process was terminated")
	}
}
func TestClosePreservesProcessForReconciliation(t *testing.T) {
	command, tree, _, done := startHelper(t, "single")
	identity, err := stopport.Identity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err = tree.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if !Alive(command.Process.Pid) {
		t.Fatal("Close terminated an unconfirmed process")
	}
	if err = stopport.Request(command.Process.Pid, identity); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("graceful stop did not complete")
	}
}
func TestFailedStartReturnsNoTree(t *testing.T) {
	tree, err := Start(exec.Command(filepath.Join(t.TempDir(), "missing.exe")))
	if err == nil || tree != nil {
		t.Fatal(fmt.Sprintf("tree=%v err=%v", tree, err))
	}
}
