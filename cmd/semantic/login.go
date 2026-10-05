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

package main

import (
	"flag"
	"fmt"
	"os"
)

// runLogin 执行 semantic login：校验账号密码后把凭据写入
// ~/.semantic/credentials.json（0600），后续 chat/sessions 免密使用。
func runLogin(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	server := fs.String("server", defaultServer, "服务地址")
	username := fs.String("username", "", "用户名（必填）")
	password := fs.String("password", "", "密码（必填）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *username == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "login 需要 --username 与 --password")
		return 2
	}

	client, err := newClient(*server, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	token, expiresAt, err := client.login(*username, *password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "登录失败:", err)
		return 1
	}
	if err := saveCredentials(credentials{
		Server: *server, Token: token, ExpiresAt: expiresAt,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "保存凭据失败:", err)
		return 1
	}
	path, _ := credentialsPath()
	fmt.Printf("登录成功，凭据已保存到 %s（有效期至 %s）\n",
		path, expiresAt.Local().Format("2006-01-02 15:04:05"))
	return 0
}

// loadValidCredentials 读取凭据并校验有效期；失败时给出可操作的指引。
func loadValidCredentials() (credentials, error) {
	creds, err := loadCredentials()
	if err != nil {
		return credentials{}, err
	}
	if creds.Expired() {
		return credentials{}, fmt.Errorf("token 已过期（%s），请重新 semantic login",
			creds.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	}
	return creds, nil
}
