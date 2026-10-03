# git-ferry-core

[![CI](https://github.com/yi-nology/git-ferry-core/actions/workflows/ci.yml/badge.svg)](https://github.com/yi-nology/git-ferry-core/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/yi-nology/git-ferry-core.svg)](https://pkg.go.dev/github.com/yi-nology/git-ferry-core)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

`git-ferry-core` 是 [git-ferry](https://github.com/yi-nology/git-ferry) 生态的业务引擎核心库：
Git 仓库镜像 / 备份 / 容灾演练 / 生命周期治理 / 组织编排等能力的实现层。
公网壳（git-ferry）与内网壳（git-ferry-intranet）都基于本库构建；本库是纯库，
除 `pkg/proclog` 显式提供进程级日志/退出辅助供壳调用外，不决定宿主进程的生命周期。

## 包布局

| 包 | 职责 |
|---|---|
| `service` | 业务服务层入口：`NewService(cfg *Config, opts ...Option)` 装配各领域服务 |
| `executor` | 同步/备份/DR 演练执行引擎、pipeline、bundle 与远端备份操作 |
| `mirror` | 镜像通道门禁、语义校验、跨实例镜像互斥 |
| `dao` | GORM 数据访问层（平台/仓库/任务/运行/审批/模板/操作日志等） |
| `model` | GORM 模型与 `AutoMigrate` 注册（启动自动建表） |
| `health` | 健康检查聚合 |
| `lock` | 进程内互斥与 redis 跨实例锁（抖动时降级为进程内互斥） |
| `tpl` | 模板存储（文件实现，可被 `service.TemplateStore` 注入替换） |
| `pkg` | 公共件：`proclog`、`textutil` 等 |

## 安装

```bash
go get github.com/yi-nology/git-ferry-core@latest
```

完整装配与调用方式参考 [git-ferry 的 `internal/corebridge`](https://github.com/yi-nology/git-ferry/tree/master/internal/corebridge)
与 [pkg.go.dev API 文档](https://pkg.go.dev/github.com/yi-nology/git-ferry-core)。

## 版本与兼容

- 版本遵循 [Semantic Versioning](https://semver.org/lang/zh-CN/)，变更记录见 [CHANGELOG.md](CHANGELOG.md)
  （格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)）。
- **`go.work` 只是本地多仓联调的覆盖**：CI 与发布均忽略 `go.work`。改动依赖本库时，
  必须显式升级 `go.mod` 中的 `require` 版本并给本库打新 tag，发布后用
  `GOWORK=off go build ./...` 自检，防止本地绿、发布红。

## 开发

```bash
make tidy   # go mod tidy
make test   # go test ./... -race -count=1
```

提交前请保证 `gofmt -l .` 为空；CI 会执行格式检查、`go vet`、`-race` 测试与 `golangci-lint`。

## 安全

漏洞报告与安全联系方式统一维护在
[git-ferry 的 SECURITY.md](https://github.com/yi-nology/git-ferry/blob/master/SECURITY.md)。
平台凭据以加密形式落库，加密密钥由调用方（壳层）配置注入，不进入本库默认值。

## License

[MIT](LICENSE)
