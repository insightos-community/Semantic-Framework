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

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/grandcat/zeroconf"

	"insightos.cn/semantic-framework/pkg/config"
)

const semanticServerMDNSService = "_semantic-server._tcp"

// startMDNSAdvertiser 只把当前 Server 的连接地址发布到局域网。设备身份仍由
// Pilot enrollment 建立，广播内容不包含 token，也不参与权限判断。
func startMDNSAdvertiser(cfg *config.Config) (*zeroconf.Server, error) {
	httpPort, err := listenPort(cfg.Server.HTTPAddr)
	if err != nil {
		return nil, fmt.Errorf("解析 HTTP 监听端口: %w", err)
	}
	wsPort, err := listenPort(cfg.Server.WSAddr)
	if err != nil {
		return nil, fmt.Errorf("解析 WS 监听端口: %w", err)
	}
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		hostname = "semantic-server"
	}
	text := []string{
		"server_id=" + hostname,
		"display_name=" + hostname,
		"api_version=1",
		"http_port=" + strconv.Itoa(httpPort),
		"ws_port=" + strconv.Itoa(wsPort),
	}
	return zeroconf.Register(hostname, semanticServerMDNSService, "local.", httpPort, text, nil)
}

func listenPort(address string) (int, error) {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 {
		return 0, fmt.Errorf("无效端口 %q", portText)
	}
	return port, nil
}
