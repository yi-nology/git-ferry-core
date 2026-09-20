package mirror

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/tools/go/packages"
)

// 验证重试策略:proxy 首次回源常有数秒延迟,失败先重试再排查。
// 包级变量以便测试收紧。
var (
	verifyAttempts  = 3
	verifyRetryWait = 2 * time.Second
)

// VerifyModule 验证 module@version 已可被 go 工具链正常消费,成功即同时
// 证明:仓库可达、版本存在、zip 拉取成功、模块 go.mod 声明的身份与
// module 一致——与在临时模块中执行 `go list -m <module>@<version>` 等价。
//
// 实现:在临时目录写入仅含 require 的 go.mod(动态数据只经文件传递),
// 由 go/packages 驱动工具链解析该模块的全部包。需要网络与 GOPROXY 配置;
// 私有模块需按 go 惯例设置 GOPRIVATE 等环境。
func VerifyModule(ctx context.Context, module, version string) error {
	if err := validateName("module", module); err != nil {
		return err
	}
	if err := validateVersion(version); err != nil {
		return err
	}

	var lastErr error
	for attempt := 1; attempt <= verifyAttempts; attempt++ {
		lastErr = verifyOnce(ctx, module, version)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt < verifyAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(verifyRetryWait):
			}
		}
	}
	return fmt.Errorf("验证 %s@%s 失败(proxy 回源可能有数秒延迟,已重试 %d 次): %w",
		module, version, verifyAttempts, lastErr)
}

func verifyOnce(ctx context.Context, module, version string) error {
	dir, err := os.MkdirTemp("", "gomod-verify-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// require 写进 go.mod 文件而非命令行参数;-mod=mod 允许工具链补齐 go.sum
	goMod := fmt.Sprintf("module verify\n\ngo 1.21\n\nrequire %s %s\n", module, version)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		return err
	}

	cfg := &packages.Config{
		Mode:    packages.NeedName | packages.NeedModule | packages.NeedFiles,
		Dir:     dir,
		Env:     append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod"),
		Context: ctx,
	}
	pkgs, err := packages.Load(cfg, module+"/...")
	if err != nil {
		return fmt.Errorf("解析模块失败: %w", err)
	}
	if len(pkgs) == 0 {
		return fmt.Errorf("未解析到任何包")
	}

	var (
		firstErr   error
		seenModule bool
	)
	for _, p := range pkgs {
		if p.Module != nil {
			seenModule = true
			if p.Module.Path != module {
				return fmt.Errorf("身份不符: %s@%s 的 go.mod 声明为 %s", module, version, p.Module.Path)
			}
			if p.Module.Version != version && p.Module.Version != "" {
				return fmt.Errorf("版本不符: 解析为 %s 而非 %s", p.Module.Version, version)
			}
		}
		for _, e := range p.Errors {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %s", e.Pos, e.Msg)
			}
		}
	}
	if !seenModule {
		return fmt.Errorf("未获取到模块信息: %w", firstErr)
	}
	if firstErr != nil {
		return firstErr
	}
	return nil
}
