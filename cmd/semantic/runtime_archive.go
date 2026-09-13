// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"insightos.cn/semantic-framework/internal/simulation"
)

// Gzip packs need no external decompressor on macOS. Only regular files and
// directories are accepted, before any data is written through an archive path.
func extractRuntimePackGzip(ctx context.Context, parent, archive string) (string, func(), error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", func() {}, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", func() {}, err
	}
	defer gz.Close()
	dir, err := os.MkdirTemp(parent, ".runtime-pack-extract-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	fail := func(err error) (string, func(), error) { cleanup(); return "", func() {}, err }
	r := tar.NewReader(gz)
	seen := map[string]bool{}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(err)
		}
		if err := validateRuntimeArchiveEntry(h.Name); err != nil {
			return fail(err)
		}
		name := filepath.Clean(h.Name)
		if seen[name] || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) {
			return fail(fmt.Errorf("unsupported or duplicate Runtime Pack entry: %s", h.Name))
		}
		seen[name] = true
		total += h.Size
		if h.Size < 0 || total > 8<<30 || len(seen) > 100000 {
			return fail(fmt.Errorf("Runtime Pack size limit exceeded"))
		}
		target := filepath.Join(dir, name)
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0700); err != nil {
				return fail(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return fail(err)
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fail(err)
		}
		_, copyErr := io.Copy(out, r)
		closeErr := out.Close()
		if copyErr != nil {
			return fail(copyErr)
		}
		if closeErr != nil {
			return fail(closeErr)
		}
	}
	// Consume the trailer so truncated archives and gzip checksum failures fail.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return fail(err)
	}
	if err := validateExtractedRuntimePackTree(dir); err != nil {
		return fail(err)
	}
	if _, err := simulation.LoadRuntimePack(dir); err != nil {
		return fail(err)
	}
	return dir, cleanup, nil
}
