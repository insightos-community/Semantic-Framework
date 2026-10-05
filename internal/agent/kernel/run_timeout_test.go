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

package kernel

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "net/http: timeout awaiting response headers" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

var _ net.Error = timeoutNetError{}

func TestWrapStreamReadErrorTimeout(t *testing.T) {
	err := wrapStreamReadError(context.DeadlineExceeded)
	if err == nil || !strings.Contains(err.Error(), "模型请求超时") {
		t.Fatalf("deadline 应改写成模型请求超时，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "timeout_seconds") ||
		!strings.Contains(err.Error(), "整次流式读取超过 HTTP 时限") {
		t.Fatalf("超时文案应说明 HTTP 时限，实际: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("应保留原始 DeadlineExceeded")
	}

	clientTimeout := errors.New("Get \"https://example\": net/http: request canceled (Client.Timeout exceeded while awaiting headers)")
	err = wrapStreamReadError(clientTimeout)
	if err == nil || !strings.Contains(err.Error(), "模型请求超时") {
		t.Fatalf("Client.Timeout 应改写成模型请求超时，实际: %v", err)
	}

	err = wrapStreamReadError(timeoutNetError{})
	if err == nil || !strings.Contains(err.Error(), "模型请求超时") {
		t.Fatalf("net.Error Timeout 应改写成模型请求超时，实际: %v", err)
	}
}

func TestWrapStreamReadErrorCanceledIsNotTimeout(t *testing.T) {
	err := wrapStreamReadError(context.Canceled)
	if err == nil || strings.Contains(err.Error(), "模型请求超时") {
		t.Fatalf("Canceled 不应报超时，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "读取模型流式帧失败") {
		t.Fatalf("Canceled 应保持读帧失败文案，实际: %v", err)
	}
}

func TestWrapStreamReadErrorOther(t *testing.T) {
	err := wrapStreamReadError(errors.New("connection reset"))
	if err == nil || !strings.Contains(err.Error(), "读取模型流式帧失败") ||
		strings.Contains(err.Error(), "模型请求超时") {
		t.Fatalf("普通错误不应改写成超时，实际: %v", err)
	}
}
