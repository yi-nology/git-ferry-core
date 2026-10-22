# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [v0.11.0] - 2026-10-22

### Changed

- **依赖地板抬到真最新**：oekit `core` v0.6.0→**v0.12.0**、`pkg` v0.11.0→**v0.21.0**、
  `concurrency` v0.10.0→**v0.20.0**、`logx` v0.7.0→**v0.7.1**、`redisx` v0.7.6→**v0.10.0**、
  go-git-platform v0.90.1→**v0.96.0**（go 指令 1.27.2→1.27.3，与 go-git-platform 对齐）。
  `pkg` 由间接依赖升为直接依赖（health/strutil 开始直接消费 `convert`/`slicex`/`stringx`）。
- **手写并发原语收编 oekit `concurrency`**（本轮主体，净减代码）：
  - `service/concurrency.go` 本地并发闸门 `chan struct{}` → `syncx.Gate` + `TryAcquire`
    （满额即拒语义与原 `select/default` 逐位一致，release 幂等）；
    信号量续期 watchdog 的手写 ticker → `loop.Run`（FirstDelay 复刻「先等一个周期」，
    退出信号由 stop channel 换成 ctx 取消）。
  - `service/health_service.go` 逐任务 channel + 信号量扇出 → `syncx.FanInLimitCtx`：
    并发上限由引擎内建、按 index 序返回、panic 落槽不丢结果；ctx 取消时不再启动新工作
    （原实现的 goroutine 会在信号量上一直排队到 ListHistory 自己超时）。
  - `service/cleanup.go` 三 goroutine + channel → `syncx.FanInErr`（逐槽 panic 收编 +
    结果按槽位序），周期清理 ticker → `loop.Run`。
  - `service/lifecycle_bg.go` 生命周期 ticker → `loop.Run`（panic 隔离：单轮失败只记日志，
    不中断循环——原实现里一次 panic 会杀死整个循环）。
  - `service/mirror_service.go` per-key 互斥表（`sync.Map` + 类型断言）→ `syncx.KeyedGate`
    （cap=1 → 每个通道一把锁，Acquire 感知 ctx 取消）。
  - `mirror/verify.go` 手写固定间隔重试循环 → `retry.Do`（退避等待感知 ctx 取消）。
- **`health/**` 工具函数收编 oekit `pkg`**：手写 `atoi` → `convert.Str2Int`、
  `toFloat` → `convert.Str2Float64`、`uniqueKeepOrder`/`uniqueStrings` → `slicex.Unique`+
  `slicex.Filter`（两处逐字重复的保序去重合并为一份口径）、手写 `minInt`/`maxInt` → 内建
  `min`/`max`。
- **`service/retry_policy.go`**：`pruneLocked` 的手写 O(n²) 选择排序 → 标准库 `slices.Sort`
  （1000 条目时 10⁶ → 10⁴ 次比较，排序键与保留策略不变）。
- **`pkg/proclog`**：`logx.SetDefault(logx.NewSlogLogger(...))` → `logx.UseSlog(...)`（等价简写）。
- **手写排序/取址收编标准库与 oekit `pkg`**：
  - `executor/bundle.go` 与 `executor/bundle_ops.go` 两处手写 O(n²) 冒泡排序
    → 标准库 `sort.Strings` / `sort.Slice`（两处是同一逻辑的复制）。
  - `executor/drdrill.go` 与壳层 `internal/metrics/metrics.go` 两份逐字同构的
    `sortedKeys` → 标准库 `slices.Sorted(maps.Keys(m))`。
  - `executor/executor.go` 的 `timePtr` → oekit `pkg/ptrx.Of`。
- **CSV 拆分跨仓统一**：core `executor` 与壳层 CLI 各有一份逐字相同的
  `splitCSV`/`splitCSVNonEmpty`，统一到 oekit `pkg/stringx.SplitCSVNonEmpty` 一份
  （oekit pkg v0.22.0 新增，见下「收编上游新原语」）。
- **分页缺省与上限单一事实源**：core `dao`、壳层 `converter.PageToOffset`、
  壳层 `mirror` 的 `parsePage` 各写一份钳制，缺省值还分 50 与 20 两种。
  现在常量只在 `dao.DefaultPageSize` / `dao.MaxPageSize` 定义一次。
- **`pkg/strutil`**：`Itoa`/`Itoa64`/`BoolFact`/`TruncateRunes` 四个函数体委托给 oekit
  `convert`/`stringx`（**签名一律不变**——本包是跨仓公开 API）。按字节 `Truncate`、
  `CSVEscape`、`SanitizeFileToken`、`SanitizePathToken` 保留本地实现：oekit 无等价物，
  且 `pathx.SanitizeFileName` 的语义相反（取最后一段 vs 保留整串），替换会撞名。
- **收编上游新原语（oekit pkg v0.22.0）**：本轮曾为消费方缺口在本地实现过
  `MatchGlob` / `IsSafeFileName` / `SplitCSVNonEmpty` 三个原语，上游已补齐同名原语，
  **本地实现全部删除、改为直接调用 oekit**（`pathx.MatchGlob` / `pathx.IsSafeFileName` /
  `stringx.SplitCSVNonEmpty`）——本地不再有第二份口径。
  两处上游实现比本地更正确：
  - `pathx.MatchGlob` 把「`*` 是通配符」的判定**前置于**字面量比较——本地实现
    顺序反了，模式里的 `*` 会被当成字面量（`MatchGlob("a*b", "a*b")` 本地返回 false、
    上游返回 true）；
  - `stringx.SplitCSVNonEmpty` 全空白输入返回 **nil**（本地返回空切片）——
    调用方用 `!= nil` 区分「没配」与「配了空」，空切片会把两者混同。

### Added

- **`dao.DefaultPageSize` / `dao.MaxPageSize`**：分页缺省与上限的常量（50 / 200）。
### Fixed

- **同一 glob 表达式在 core 内有三份语义各异的实现**（此前必现的行为不一致）：
  `executor/advanced.go` 的递归版（`*` 跨 `/`）、`health/filter.go` 的
  `path.Match` 版（不跨 + 子串兜底）、`service/provider_helper.go` 的
  `filepath.Match` 版（不跨）。现收敛为 `strutil.MatchGlob` 一份，口径为
  **`*` 跨 `/`、`?` 匹配一个 rune**。三处行为变化：
  - 平台仓库过滤（`provider_helper.go`）此前用 `filepath.Match`（不跨 `/`），
    而匹配对象是仓库全名 `owner/repo`——于是 `*` 这类模式**永远匹配不到**，
    等于配了等于没配（`exclude_globs: ["*"]` 拦不住任何仓库）。现在按直觉生效。
  - `health` 过滤的 `*foo*` 子串兜底此前与 `path.Match` 重复且对中间含 `*`
    的模式（如 `a*b`）按字面量找（几乎永不命中）；现在按 glob 正常语义匹配。
  - `?` 由**按字节**切改为**按 rune** 切——此前中文名会被切出半个字符。
- **bundle 名可逃逸到父目录**（`service/dr_service.go` 的 `RunDRDrill` / `BatchDRDrill`）：
  原校验是 `filepath.Base(name) != name`，而 `filepath.Base("..") == ".."` —— `".."`
  会被放行，随后 `filepath.Join(backupDir, name)` 把路径指到 backup 目录的**父目录**。
  改用 `strutil.IsSafeFileName`：拒 `"."` / `".."` 段、绝对路径、Windows 盘符与 UNC。
  同款校验在壳层 `biz/handler/git_sync/backup_service.go`（verify / restore bundle）
  也是同一个洞，一并收敛到本函数。
- **分布式信号量续期可复活已过期槽**（`lock` 包原手写实现）：原 `renewScript` 只判
  「成员存在于 ZSET」而不判是否已过期，而 `Acquire` 清理过期槽后其配额可能已被其他持有者
  接管——此时 `Renew` 把过期槽"复活"即导致全局并发超限。改用 oekit `redisx/distlock.Semaphore`
  后，续期要求 `score > now`（只续未过期的槽），且 `Acquire` 对同一 identifier 幂等刷新。
  Redis key 布局保持 `git-sync:semaphore:<key>` 不变（滚动升级期新老实例看同一把信号量），
  并新增两条 miniredis 回归测试钉住 key 布局与「过期槽不得被复活」。

### Removed

- **`lock.LocalLock`（进程内互斥）**：三仓均无生产调用方（仅自身测试），且其「无限指数退避
  直到 ctx 取消」的锁语义 oekit `retry`/`poll` 均表达不了（两者都是有界重试），留着只会
  被误用成第二套锁语义。连同其测试一并删除。

### Breaking

- `lock.Semaphore` 的 `Renew` 返回值由 `error` 改为 `(bool, error)`——`false` 表示槽已丢失
  （过期回收或已被释放），调用方须据此停工而非继续续期；`Cleanup` 返回值由 `error` 改为
  `(int64, error)`（被清理的槽数）。实现改为内嵌 `*distlock.Semaphore`。
- `lock.NewSemaphore` 的 client 参数由 `*redis.Client` 放宽为 `redis.UniversalClient`。

## [v0.10.2] - 2026-10-18

### Changed

- **依赖升 go-git-platform v0.90.1、oekit concurrency v0.10.0 / logx v0.7.0 / redisx v0.7.6**(地板对齐;
  concurrency 新增 breaker 三态/计数触发、retry DelayHint/JitterRatio、async PanicError/Runner、
  loop/syncx/budget 等均为增量,本仓所用 GoSafe/retry.Do API 不变)。
- **适配 logx v0.7.0 破坏变更**:`ParseLogLevel` 签名改 `(Level, error)`——proclog 改用
  `ParseLogLevelOr(level, LevelInfo)` 免判空形态,语义不变(配置错退 Info)。

## [v0.10.1] - 2026-10-17

### Changed

- **依赖升 go-git-platform v0.89.0 → v0.90.0、oekit/logx v0.5.0 → v0.5.1**(地板对齐,
  生产代码零改动)：
  - SDK v0.90.0 随 oekit 新地板（pkg v0.11.0 / concurrency v0.5.4 / logx v0.5.0）对齐,
    transport 在 Debug 被级别过滤时不再求值响应体拷贝等 kv 参数(`DebugEnabled` 守卫)。
  - logx v0.5.1 修复 `NewStdLogger` 无参默认级别 DEBUG 的 iota 陷阱(#11)——未设
    `LOG_LEVEL` 时 oekit 组件内部日志会全量打进生产；本仓 proclog 一直显式设级,
    实际未受影响,升级以对齐契约。logx 另新增 `NewAuditLogger`(审计留痕不被降级),暂无使用点。

## [v0.10.0] - 2026-10-17

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

- **依赖升 go-git-platform v0.88.1 → v0.89.0 + oekit 新基线**:concurrency v0.4.0 → v0.5.2
  (retry 指数退避/Jitter、loop、GoSafeLabeled 等纯增量,GoSafe/retry.Do 签名未动)、
  logx v0.3.0 → v0.5.0(`Delegating` 改单参——SDK v0.89.0 已同步适配其零参调用;
  Sink 迁嵌套模块 `logx/sink`,本仓未使用)、间接 pkg v0.6.0 → v0.9.3。生产代码零改动,
  `go mod tidy` exit 0;`go test -race ./...` 10 包全绿。
- **依赖升 go-git-platform v0.76.0 → v0.88.1**(12 个 0.x minor,SDK 侧已接入 oekit
  pkg/errors·concurrency·logx,与本仓同一套版本线)。随之 **Go 版本线 1.26.3 → 1.27.2**
  (SDK go.mod 要求;CI 矩阵同步 `'1.27'`),go-git v5.19.3、gitlab client-go v3.18.0、
  x/crypto v0.58.0。接口适配仅一处:`ChangeRequestManager.ListCRs` 的 total 改为 `*int`
  (nil=平台未给总数),生产代码本就忽略该值,只改测试假对象。
  **随 SDK 带入的行为变更**(本仓代码未改,契约随 SDK):
  - webhook 验签**空密钥从"放行"改为拒绝**(SDK 0.77 安全加固:空 HMAC 密钥可伪造载荷)。
    经 git-ferry 注册的 webhook 都会写入密钥;手工在平台侧配置且未填密钥的回调将被拒
    (壳层返 401),需在平台与仓库两侧补齐同一密钥。
  - 分页触顶(50 页预算)从"静默截断"改为报 `provider.ErrPageBudgetExceeded`
    ——本仓 `CollectBounded`/`EachBounded` 调用点已按错误处理。
  - SDK 错误文本形状变化(动态值移到 kv 后缀);本仓 `executor.ClassifyError` 的关键字
    分类与哨兵判定均不受影响(测试全绿)。
- **接入 oekit 基础库**（替换自写并发/锁原语，依据 <https://oekit.github.io/llms.txt>）：
  - **跨实例互斥锁 → `github.com/oekit/redisx/distlock`**：删除 `lock.RedisLock`
    （自写 SetNX/Lua 解锁/CAS 续期/`lockValues` 映射共 ~180 行），`redisGuard` 的任务锁与
    cron 锁、`MirrorService` 的通道锁改走 `distlock.RedisLocker`；任务锁续期由库的
    `WithAutoRenew` 承担（`WithMaxAutoRenew(-1)` 取消 10×TTL 默认上限——同步任务可跑数小时），
    watchdog 只剩信号量槽续期。key 布局固定为 `git-sync:lock:<key>`（`lock.LockKeyPrefix`），
    滚动升级期间新老实例共用同一把锁；`lock` 包保留 ZSET `Semaphore`、`LocalLock`，
    新增 `NewRedisClient` / `NewLocker` 构造器。
  - **裸 `go func` → `github.com/oekit/concurrency/async.GoSafe`**：service 层 15 处后台
    goroutine（webhook 规则应用、异步任务、镜像执行、事件分发、清理、生命周期、健康评分
    fan-out、健康检查 ping）统一经 GoSafe 启动并 recover，删除 6 处手写 recover 样板；
    健康评分 fan-out 改为 defer 回传结果，子任务 panic 不再卡死聚合方。
  - **push 重试 → `github.com/oekit/concurrency/retry.Do`**：线性退避改为「第 i 次失败后等待
    i×RetryBackoffMs」（此前为 (i+1)×），ctx 取消统一映射为 `retry aborted`。
  - **`pkg/proclog.Setup`** 顺带接管进程内 oekit 组件：`async.SetPanicHandler` 走 slog；
    `logx.SetDefault` 以 `WithHandler` 注入同一个 slog.Handler（logx v0.3.0）——oekit
    组件内部日志（distlock 续期告警等）与业务日志同格式同级别落 stdout，service 字段取
    可执行文件名，text/json 配置对组件日志同样生效。
- **`ResolveOrgTarget` 签名变更**：改为 `(sourceOwner, sourceRepo, OrgMapOptions)`，
  原多参数版本只有测试引用（生产零调用）。
- **分支名校验放行 glob 字符 `*?[]`**：`executor` 明确以
  `strings.ContainsAny(spec, "*?[")` 识别多分支规格，而 `CreateTask` 的
  `validateBranchName` 此前拒绝 `*`——导致**组织导入/组织镜像建任务一直静默失败**。

### Removed

- **`lock.RedisLock` / `lock.DistLock`**：被 oekit `distlock` 取代（见 Changed）；
  对应的 `redis_lock_test.go`（依赖本机 6379 的集成测试）一并删除，锁的正确性由库自身
  测试 + 本仓 `TestNewLocker_KeyLayout_miniredis`（key 布局与 owner 校验）与
  `TestRedisGuard_*`/`TestMirrorLockChannel_*` 覆盖。
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
