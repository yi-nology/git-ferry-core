# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Added

- **业务数据入表**（承接 v0.9.0 的存储下沉，补完多实例正确性）：
  - `force_push_approvals` 表 + `dao.ForcePushApprovalDAO`；`ForcePushStore` 改为
    表存储（读-改-写有实例内互斥），旧 `<backup_dir>/force-push-approvals.json`
    在**表为空**时一次性导入（幂等）；JSON 字段与历史文件逐字兼容。
  - `templates` 表（`tpl.Template` 增加 gorm 标签，json 契约不变）+ `dao.TemplateDAO`；
    新增 `service.TemplateStore` 接口，生产为 DB 实现，测试/过渡可注入文件实现
    （`SetTemplates(TemplateStore)`）；旧 `data/templates.json` 空表时一次性导入。
  - `AutoMigrate` 注册上述两表（启动自动建表，无需手工迁移）。
- **`pkg/proclog`**：`Setup(level, format)` 与 `ExitOnFail(msg, err)`——
  公网壳与内网壳此前各复制一份逐字相同的实现，两者都依赖 core 故收敛于此
  （core 自身不调用：库不应决定宿主进程日志与退出行为）。
- **测试补账**（承接 v0.9.0）：`mirror_service`（通道 CRUD、凭据加密落库、入参校验）、
  `backup_service`（bundle 列举/过期清理/legal hold/元数据快照轮转）、
  `dr_service`（RPO 报表、演练历史哈希链篡改检出、冷备清单校验）、
  `lifecycle_service`（AutoDiscover 只读发现/导入后比对、DetectDrift 空路径，
  平台 API 用 httptest 伪造）、模板与审批的 DB 存储 + 旧文件迁移。

### Fixed

- **`UpdateMirrorTarget` 凭据空串语义**：文档写「空串=保持原凭据」，实现却在
  `validateTargetInput` 直接拒绝（前端改 URL 必须重传 token）。拆分
  `requireCredential` 参数，更新路径留空即保留原密文；新建路径仍强制必填。

- **装配期依赖注入**：`NewService(cfg, opts...)` + `Option`——`WithDB`（外部
  `*gorm.DB`，`Stop()` 不再关闭注入的连接池）、`WithProviderHooks`、
  `WithForcePushApprover`。provider 钩子由包级全局改为**随 Service 实例走**。
- **错误→HTTP 分类契约**：`ErrorClass` + `Classify(err)`，配套哨兵
  `ErrPlatformNotFound` / `ErrTargetPlatformNotFound` / `ErrStarredUnsupported` /
  `ErrTemplateNotFound` / `ErrTemplateCycle` / `ErrMetadataValidation`（壳层据此选状态码）。
- **org 映射收敛为唯一实现**：`OrgMapOptions{PreserveRedirect, MixedOrgToTarget,
  RequireTarget}` 表达三份历史语义差异 + `ParseStrategy`；修 `IsPersonalOwner`
  恒 false 的死分支。
- **metadata 备份/回灌引擎**（自壳层下沉）：`BackupMetadata` /
  `ListMetadataBackups` / `RestoreMetadata` / `SampleMetadataVerify` /
  `MetadataBackupDir` / `BackupGists` / `DownloadReleaseAssets`，含 manifest、
  kind 分发、截断与历史文件格式。
- **health 评分**（整包自壳层 `internal/health` 平移为 `health/`）+
  `Service.HealthSnapshot`（含 summary 聚合）。
- **运维聚合查询**：`OpsTrends`、`OpsTodo`、`TaskService.RecentRuns`。
- **组织导入/镜像编排**：`ImportPublicOrg` / `BulkMirrorOrg` / `ListStarredRepos` /
  `RewriteRepoURL`（列表、过滤、映射、批量建仓建任务与统计）。
- **同步策略模板**（自壳层平移为 `tpl/`）：`Service.Templates` /
  `SetTemplates` / `PreviewTemplate` / `ApplyTemplate` / `RepoInventory`，
  新增 `cfg.Templates.Path`（默认 `data/templates.json`）。
- **force-push 审批存储**：`ForcePushStore`（JSON 字段与历史文件逐字兼容），
  `Service.ForcePushApprovals()`，并**默认接线为执行器 Approver**（`WithForcePushApprover` 可覆盖）。
- **运行完成事件**：`SubscribeRuns(fn) (cancel)` + `RunEvent`，在
  `RunTaskAsync` / `RunTaskWithTrigger` 收尾广播（cron/webhook/手动全覆盖），
  每订阅者独立 goroutine + recover。
- **失败自动补偿**：`RetryTracker.ShouldRetry(run, AutoRetryPolicy)`（次数/冷却/淘汰）。
- **mirror 通道跨实例互斥**：配 redis 时 `mirror-channel-<id>` 抢锁（TTL 30min），
  被其它实例占用直接报忙碌；redis 抖动降级为进程内互斥；release 幂等。
- **`pkg/deploykey`**：`Generate(comment)`（Ed25519 部署密钥对）。
- **`pkg/strutil`**：`Truncate` 改 UTF-8 安全截断；新增 `TruncateRunes` /
  `SanitizePathToken` / `BoolFact`。
- 测试补账：`orgmap`（三份语义表驱动）、`Classify`、`events`/`retry`（-race）、
  `forcepush_store`（含旧文件兼容）、`mirror_lock`（跨实例/本地/降级）、
  `provider_helper`（RepoImportFilter）、`ops_query`、`template_service`、`strutil`。

### Changed

- **`ResolveOrgTarget` 签名变更**：改为 `(sourceOwner, sourceRepo, OrgMapOptions)`，
  原多参数版本只有测试引用（生产零调用）。
- **分支名校验放行 glob 字符 `*?[]`**：`executor` 明确以
  `strings.ContainsAny(spec, "*?[")` 识别多分支规格，而 `CreateTask` 的
  `validateBranchName` 此前拒绝 `*`——导致**组织导入/组织镜像建任务一直静默失败**。

### Removed

- **`SetProviderHooks`**（包级全局 setter）：无法转发到实例，改由
  `WithProviderHooks` 注入；全仓仅壳层 `main.go` 一处调用，已同步改造。

### Fixed

- **`OpsTrends` 恒返回空序列**：原实现 `ListHistory(ctx, "", 0, 2000)` 实际是
  `task_key = ''` 等值匹配，而 `CreateRun` 总写入真实 taskKey → 改用既有但未
  暴露的 `SyncRunDAO.FindRecent`（新增 `TaskService.RecentRuns` 门面）。

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

## [0.8.1] - 2026-10-02

### Changed

- 依赖 go-git-platform v0.72.0 → **v0.75.0**；分页迁移到 v0.73 收敛后的唯一
  分页面：`fetchAllPlatformRepos` 改 `provider.CollectBounded`（`ListAllPages`
  已被平台删除——空页终止语义，短页≠末页，防服务端压缩页大小时提前停；
  撞页预算报 `ErrPageBudgetExceeded` 而非静默截断）。

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
