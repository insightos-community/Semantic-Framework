package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"insightos.cn/semantic-framework/internal/install"
)

// build 只生成制品，不读取登录凭据，也不连接 Server 或修改 Robot 绑定。
func runBuild(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: semantic build <源码目录> --output <安装包.zip> [--wheel-dir <目录>] [--offline]")
		return 2
	}
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	output := fs.String("output", "", "输出安装包 ZIP")
	wheelDir := fs.String("wheel-dir", "", "构建复用的离线 Wheel 目录")
	offline := fs.Bool("offline", false, "仅从 Wheel 缓存收集依赖")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *output == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "请指定 --output <安装包.zip>")
		return 2
	}
	if *wheelDir != "" {
		_ = os.Setenv("SEMANTIC_BUILD_WHEELHOUSE", *wheelDir)
	}
	if *offline {
		_ = os.Setenv("SEMANTIC_BUILD_OFFLINE", "1")
	}
	if err := buildSourcePackage(context.Background(), args[0], *output, false); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if pkg, err := install.InspectArchive(*output); err == nil && pkg.Kind == "runtime" {
		digest, err := install.ArchiveDigest(*output)
		if err == nil {
			err = os.WriteFile(*output+".sha256", []byte(digest+"  "+filepath.Base(*output)+"\n"), 0640)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	fmt.Println("已构建", *output)
	return 0
}

// 两个 CLI 入口共享构建分派，避免 install 和 build 各自维护一套配方逻辑。
// 仅从 Skill 源码直接安装时生成开发快照；交付构建保留 SKILL.md 声明版本。
func buildSourcePackage(ctx context.Context, source, output string, skillSnapshot bool) error {
	progress := func(message string) { fmt.Println(message) }
	if _, err := os.Stat(filepath.Join(source, "semantic-source.yaml")); err == nil {
		return install.BuildSource(ctx, source, output, progress)
	}
	if _, err := os.Stat(filepath.Join(source, "semantic-package.yaml")); err == nil {
		return install.BuildInstallPackage(source, output)
	}
	if _, err := os.Stat(filepath.Join(source, "semantic-component.yaml")); err == nil {
		return install.ZipDirectory(source, output)
	}
	var body []byte
	var err error
	if skillSnapshot {
		body, err = install.PackSkillSource(source)
	} else {
		body, err = install.PackSkillRelease(source)
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0750); err != nil {
		return err
	}
	return os.WriteFile(output, body, 0640)
}
