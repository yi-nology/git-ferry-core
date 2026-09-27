# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [Unreleased]

## [0.5.0] - 2026-09-27

### Added

- **仓库导入过滤** `RepoImportFilter`: 排除 archived/fork、最低 star、
  语言、FullName glob;`SyncPlatformReposFiltered` 透传。
- **部分克隆** `sync.partial_clone` (`blob:none`/`tree:0`) 降冷备体积。
- **子模块递归** 任务字段 `submodules`。
- **S3 异地冷备** `sync.backup_s3`: bundle 上传 S3 兼容存储
  (SigV4,path-style,兼容 MinIO/OSS/AWS)。
- **git bundle 冷备** `git_bundle` + `sync.backup_dir` + `backup_keep` 轮转。
- **Wiki 同步** `sync_wiki`: `.wiki.git` 推导,未启用则跳过。
- **依赖迁移**: `git-platform-sdk` → `go-git-platform` v0.64.0。

### Changed

- `branchfilter.New` 适配双返回值;非法模式跳过不拖垮链路。

## [0.4.7] - 2026-09-27

### Added

- bundle 备份 keep-N 轮转 (`sync.backup_keep`)。

## [0.4.5] - 2026-09-27

### Added

- Wiki 仓库同步 (`SyncWiki`)。

## [0.4.4] - 2026-09-27

### Added

- Platform SSH 主机指纹钉扎 `ssh_host_key_fingerprint` / `ssh_known_hosts_path`。

## [0.4.3] - 2026-09-27

### Added

- **分歧保护** `KeepDivergent`: force 推送前检测目标独有提交,默认拒绝覆盖。
- **LFS 同步** `GitLFS`。
- **推送 prune** `GitPushPrune`。
- **多分支 glob** `SourceBranch` 支持 `release/*`。
- 错误分类 `divergent` / `conflict`。
