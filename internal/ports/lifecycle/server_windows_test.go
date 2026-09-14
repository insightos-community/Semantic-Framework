//go:build windows

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
package lifecycle

import (
	"gopkg.in/yaml.v3"
	processport "insightos.cn/semantic-framework/internal/ports/process"
	stopport "insightos.cn/semantic-framework/internal/ports/stop"
	"insightos.cn/semantic-framework/pkg/config"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func port(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	return l.Addr().String()
}
func TestNativeServerInitializeRestartAndGracefulStop(t *testing.T) {
	bin := os.Getenv("SEMANTIC_NATIVE_BIN")
	if bin == "" {
		t.Skip("set SEMANTIC_NATIVE_BIN to built executables")
	}
	root := filepath.Join(t.TempDir(), "中文 instance with spaces")
	configPath := filepath.Join(root, "configs", "semantic-server.yaml")
	init := exec.Command(filepath.Join(bin, "semantic.exe"), "init", "-c", configPath)
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, output)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.HTTPAddr = port(t)
	cfg.Server.WSAddr = port(t)
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	client := http.Client{Timeout: time.Second}
	for cycle := 0; cycle < 2; cycle++ {
		command := exec.Command(filepath.Join(bin, "semantic-server.exe"), "-c", configPath)
		command.Dir = root
		log, err := os.Create(filepath.Join(root, "server.log"))
		if err != nil {
			t.Fatal(err)
		}
		command.Stdout = log
		command.Stderr = log
		tree, err := processport.Start(command)
		if err != nil {
			log.Close()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		t.Cleanup(func() { tree.Kill(); tree.Close(); log.Close() })
		ready := false
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			response, e := client.Get("http://" + cfg.Server.HTTPAddr + "/api/v1/system/healthz")
			if e == nil {
				response.Body.Close()
				if response.StatusCode == 200 {
					ready = true
					break
				}
			}
			select {
			case err = <-done:
				output, _ := os.ReadFile(log.Name())
				t.Fatalf("server exited: %v\n%s", err, output)
			default:
			}
		}
		if !ready {
			output, _ := os.ReadFile(log.Name())
			t.Fatalf("server not ready\n%s", output)
		}
		identity, err := stopport.Identity(command.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if err = stopport.Request(command.Process.Pid, identity); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-done:
			if err != nil {
				output, _ := os.ReadFile(log.Name())
				t.Fatalf("stop: %v\n%s", err, output)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("graceful stop timed out")
		}
		tree.Close()
		log.Close()
		if err = os.Rename(cfg.Store.SQLitePath, cfg.Store.SQLitePath+".stopped"); err != nil {
			t.Fatal("database still locked after shutdown:", err)
		}
		if err = os.Rename(cfg.Store.SQLitePath+".stopped", cfg.Store.SQLitePath); err != nil {
			t.Fatal(err)
		}
	}
}
