# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [Unreleased]

## [0.6.1] - 2026-09-29

### Added

- **`ExportDrillHistory`**: 演练历史导出 JSON/CSV(含哈希链 hash)。
- **生命周期后台**: `sync.auto_discover_interval_minutes` 自动发现 + 冷备 retention 清理。
- **GitHub App 全链路联调测试**: JWT → installation token → 缓存 → provider。

## [0.6.0] - 2026-09-29

### Added

- **灾备闭环**: `executor.RunDRDrill` / `BatchDRDrill`(恢复+fsck+refs 比对+RTO),
  演练历史 JSONL 哈希链;`BuildManifest`/`VerifyManifest`(SHA256+Merkle Root);
  `Service.RPOReport` RPO/RTO 观测。
- **多目的地扇出** `executor.FanoutUpload`: s3/webdav/azure/local,`sync.backup_destinations`。
- **冷备加密** `executor.EncryptFile`/`DecryptFile`(AES-256-GCM),`.bundle.enc` 恢复自动解密。
- **生命周期**: `Service.AutoDiscover` / `DetectDrift`;任务 `force_push_policy`
  (`allow|block|backup_on_demand`,覆盖前可打回滚快照)。
- **治理**: 审计哈希链 `prev_hash/entry_hash` + `VerifyAuditChain`;
  `Service.CleanupExpiredBackups`(retention + legal_hold)。
- **GitHub App**: `MintGitHubAppJWT` / `FetchGitHubInstallationToken` /
  `ResolvePlatformToken`(installation token 缓存);平台字段
  `github_app_id/github_installation_id/github_private_key`(私钥加密入库)。

## [0.5.1] - 2026-09-29

### Added

- **冷备闭环 API**: `ListBundles` / `VerifyBundle` / `RestoreBundle`(`executor` + `Service`)。

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
