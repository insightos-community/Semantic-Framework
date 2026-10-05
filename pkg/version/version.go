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

package version

// Version 是框架的语义版本号，由 Makefile 中 -ldflags 在编译时注入。
// 直接以源码运行（go run）时使用当前开发版本，正式构建仍由 Makefile 注入。
var Version = "0.5.0-dev"

// String 返回当前版本号字符串，供 HTTP 接口与 CLI 输出使用。
func String() string {
	return Version
}
