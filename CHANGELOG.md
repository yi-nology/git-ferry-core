# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [0.7.0] - 2026-10-01

### Added

- **分支过滤**：`SyncTask.include_branches`（glob 白名单）与
  `exclude_ref_patterns`（默认忽略 refs/pull/* 等）；执行前过滤 refs。
- **post-exec 钩子** `sync.post_exec_script`：注入 GITFERRY_TASK/RESULT/RUN_ID/TRIGGER。
- **冷备 zip** `sync.backup_format: bundle|zip`（keep 轮转）。
- **org 映射辅助** `service/orgmap.go`。

### Changed

- **内聚重构**（随 v0.7.0 首发，CHANGELOG 补记）:
  - `GetMirrorVersions` 拆为 `loadTagCommits` / `buildVersionCells` / `assembleVersionMatrix`(纯函数可单测)。
  - `SyncPlatformReposFiltered` 拆为 `planRepoUpsert` + `persistRepoPlan` + `indexExistingByKey`。
  - `NewService` 经 `initDB`/`initDAOs`/`appDAOs` 装配,压缩样板。
  - `Execute` 改为 Stage 流水线;`FanoutUpload` 为 Uploader 策略注册表。

## [0.7.1] - 2026-10-01

### Added

- **ForcePushApprover 注入** `Service.SetForcePushApprover`：壳层 force-push 审批流接入。
- **backup_remote** 相关 executor 扩展（详见 executor/backup_remote.go）。

## [0.7.2] - 2026-10-01

### Changed

- **认证路径重构**：`executor/auth.go` 集中构建 git 凭证；令牌经 go-git-platform
  的临时 credential helper / GIT_ASKPASS 注入（**不进 argv / environ 明文**），
  临时凭证目录 RAII 清理。SSH 密钥内容同样经 0600 临时文件 + `GIT_SSH_COMMAND`。
  安全约定见 `executor/auth.go` 注释。
- 依赖 go-git-platform v0.64.0 → **v0.68.2**（credential helper 落地）。
  不升此依赖则认证路径仍走旧的 argv 传参。

## [0.8.0] - 2026-10-02

### Added

- **`Service.PushTaskBackup`**：把任务 mirror 工作区推到任意备份远端（临时 remote
  `backup-manual` + `gitbackend.Push`，凭证经 `executor.BuildRepoAuth`，取不到源仓库退
  AuthNone）。壳层 `POST /ops/push-backup` 改走此 API，https 私有仓可带 token 推送
  （原裸 `exec git push` 无凭证必败），并接入平台 transport 的 429/5xx 重试。
- **provider 收口导出**：`Service.ProviderForPlatform(p, tokenOverride)`
  （Manager 缓存 + GitHub App token 解析 + repo token 覆盖，壳层不再手拼
  `Config+NewProvider`）；`service.SetProviderHooks` 供壳层装限流指标 hook。
- **`executor.BuildRepoAuth(repo, p)`**：repo/platform token → AuthConfig 的
  唯一实现（原 executor.authConfig 与 MirrorService.repoAuth 两份平行逻辑合一，
  mirror 路径自此获得 SSH 指纹钉扎/knownHosts 校验）。

### Changed

- **裸 git 收口到 go-git-platform**：分歧检测改 `GetBranchSyncInfo` /
  `GetCommitsBetween`（rev-list/log 方向与格式已核对等价）；分支列举改
  `ListLocalBranches`；rev-parse 改 `RevParse`；ls-remote / for-each-ref /
  bundle clone 等改 `RunRaw`（白名单外如 bundle/fsck/update-ref/lfs 经
  `runGitThroughBackend` 回落裸 exec）；`backup_remote` 整块改
  `AddRemote/RemoveRemote/Push` 并带凭证。
- **GitHub App 下沉**：`MintGitHubAppJWT` / `FetchGitHubInstallationToken` 变
  平台 `githubapp` 包薄封装（导出签名不变，缓存/ResolvePlatformToken 保留）。
- **mirror 的 go-git auth 转换下沉**：删本地 `goGitAuth`，改用平台
  `gitbackend.TransportAuth`（同构逻辑 + SSH hostkey 回调）；远端 tag 枚举
  仍走 go-git 直调（平台无对应能力）。
- `providerConfig` 默认开启 `RetryConfig=DefaultRetryConfig()`（429/5xx）；
  `fetchAllPlatformRepos` 改 `provider.ListAllPages`（保留 CloneURL/FullName 去重）。
- `MirrorService.targetAuth` 改用平台构造器 `NewHTTPBasicAuth` / `NewSSHKeyContentAuth`。
- 依赖 go-git-platform v0.68.2 → **v0.72.0**（githubapp/四能力/分页/403 重试，
  连带 v0.69 传递依赖保鲜：gitea.dev/sdk v1.3.0、gitlab client-go v3.15.0 等）。

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
