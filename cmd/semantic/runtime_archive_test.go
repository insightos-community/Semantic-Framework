// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeGzipRejectsUnsafeEntries(t *testing.T) {
	for _, header := range []*tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg},
		{Name: "/absolute", Typeflag: tar.TypeReg},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../escape"},
		{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "../escape"},
	} {
		t.Run(header.Name, func(t *testing.T) {
			parent := t.TempDir()
			archive := filepath.Join(parent, "test.tar.gz")
			f, err := os.Create(archive)
			if err != nil {
				t.Fatal(err)
			}
			gz := gzip.NewWriter(f)
			w := tar.NewWriter(gz)
			if err := w.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			w.Close()
			gz.Close()
			f.Close()
			if _, cleanup, err := extractRuntimePackGzip(context.Background(), parent, archive); err == nil {
				cleanup()
				t.Fatal("unsafe entry accepted")
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) != 1 {
				t.Fatalf("failed extraction left files: %v", entries)
			}
		})
	}
}
