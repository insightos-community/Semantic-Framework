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

package bootstrap

import "testing"

func TestListenPort(t *testing.T) {
	for _, test := range []struct {
		address string
		want    int
	}{
		{address: ":8080", want: 8080},
		{address: "127.0.0.1:18080", want: 18080},
		{address: "[::]:8081", want: 8081},
	} {
		got, err := listenPort(test.address)
		if err != nil || got != test.want {
			t.Fatalf("listenPort(%q)=%d,%v want=%d", test.address, got, err, test.want)
		}
	}
}

func TestListenPortRejectsMissingPort(t *testing.T) {
	if _, err := listenPort("localhost"); err == nil {
		t.Fatal("缺少监听端口时必须拒绝 mDNS 广播")
	}
}
