// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package simulation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPRuntimeClientReadsOnlyGLBVisualAssets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/visual-assets/r1_pro_chassis/3.glb" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "model/gltf-binary")
		_, _ = w.Write(append([]byte("glTF"), make([]byte, 8)...))
	}))
	defer server.Close()

	client := NewHTTPRuntimeClient(server.URL, server.Client())
	content, mediaType, err := client.VisualAsset(
		context.Background(), "r1_pro_chassis", "3",
	)
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != "model/gltf-binary" || string(content[:4]) != "glTF" {
		t.Fatalf("GLB 内容或类型错误: media=%q content=%q", mediaType, content)
	}
	if _, _, err := client.VisualAsset(context.Background(), "../secret", "1"); err == nil {
		t.Fatal("visual_id 不能接受路径")
	}
}

func TestHTTPRuntimeClientRejectsNonGLBVisualContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("not-a-glb"))
	}))
	defer server.Close()

	client := NewHTTPRuntimeClient(server.URL, server.Client())
	if _, _, err := client.VisualAsset(context.Background(), "box", "1"); err == nil {
		t.Fatal("非 GLB Content-Type 必须被拒绝")
	}
}
