package main

import (
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
)

func runUninstall(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: semantic uninstall <组件ID> --project <项目ID>；semantic uninstall runtime --id <安装ID> -c <配置>")
		return 2
	}
	if args[0] == "runtime" {
		return runRuntime(append([]string{"uninstall"}, args[1:]...))
	}
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	server := serverFlag(fs)
	project := fs.String("project", "", "导入组件的项目 ID")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *project == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "请指定 --project <项目ID>")
		return 2
	}
	creds, err := loadValidCredentials()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	client, err := newClient(resolveServer(*server, creds), creds.Token)
	if err == nil {
		err = client.doJSON(http.MethodDelete, "/api/v1/projects/"+url.PathEscape(*project)+"/components/"+url.PathEscape(args[0]), nil, nil)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("组件已卸载，导入原包与历史记录保留。")
	return 0
}
