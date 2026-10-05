package main

import (
	"flag"
	"fmt"
	"os"
)

// runSessions 执行 semantic sessions：列出本人全部会话（按最近活跃倒序，
// 与服务端返回序一致）。
func runSessions(args []string) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	server := serverFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	creds, err := loadValidCredentials()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	client, err := newClient(resolveServer(*server, creds), creds.Token)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	sessions, err := client.listSessions()
	if err != nil {
		fmt.Fprintln(os.Stderr, "查询会话列表失败:", err)
		return 1
	}
	if len(sessions) == 0 {
		fmt.Println("（暂无会话）")
		return 0
	}
	for _, s := range sessions {
		fmt.Printf("%s\t%s\t%s\n", s.ID, s.Title,
			s.UpdatedAt.Local().Format("2006-01-02 15:04:05"))
	}
	return 0
}
