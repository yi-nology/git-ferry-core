# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [v0.16.1] - 2026-10-25

### Fixed

- **已存在的 pending 审批必须刷新到当前目标状态**： 见到同
  task+branch 已有 pending 就跳过 ⇒ 目标侧此后又变了（独有提交从 1 个变成 50 个）时，
  旧 pending 仍挂着、绑的还是**旧状态** ⇒ 运维批的是一份过期的描述。
  修法：已有 pending 时刷新它（reason / target_ref / created_at）。

## [v0.16.0] - 2026-10-25

### Fixed

- **backup_on_demand 兑现不了时 fail-fast，且不能被审批绕过**（`executor`）：该策略要求
  覆盖前先打回滚快照，但没配 `sync.backup_dir` 就打不了。此前**静默按 block 处理**，
  并落一条 pending 审批 ⇒ 运维一次"放行"就会在**没有备份**的情况下覆盖目标侧独有提交 ——
  那正是这个策略要防的数据丢失。修法：直接报配置错并**不产生任何审批申请**；
  配置没配好就该是配置错，不该能被审批绕过。
- **强推放行绑定到"被批准时的目标状态"**（`executor` / `service` / `dao` / `model`）：
  审批是"针对这一次分歧"的授权。目标侧若在审批之后又前进（独有提交从 1 个变成 50 个），
  旧审批不该继续适用 —— 否则一次"批 1 个独有提交"会变成"授权覆盖此后的一切"。
  修法：审批记录加 `target_ref`（申请时目标分支的 HEAD SHA）；判定时目标 SHA 变了
  ⇒ 旧放行不再适用，必须重新审批。
  （`ForcePushApprover` 接口的 `IsForcePushApproved` / `RequestApproval` 各加一个
  `targetRef` 参数 —— 外部实现方需同步。）
- **漂移检测给出真实的 ahead/behind**（`service/lifecycle_service.go`）：此前
  `rev-list --count A..B` 要求两个 sha 都在本地对象库里，而远端那个 sha（ls-remote 拿到的）
  没被 fetch 过 ⇒ 命令直接失败，旧实现静默给 0/0 ⇒ "漂移了多少"这个最该有的信息永远是空的。
  修法：先把源端分支 fetch 下来（带凭证，私有仓必须）再算；fetch 失败则明说原因。
  同时把比对分支从 `TargetBranch` 改成 `SourceBranch` —— 工作区里的本地分支是克隆**源端**时
  检出的那个分支，用 TargetBranch 去找在源/目标分支不同的任务上必然找不到
  （恒报 "cannot resolve refs"）。

## [v0.15.0] - 2026-10-24

### Fixed

- **强推放行可撤销**（`service` / `dao` / `model`）：放行是"人工授权覆盖目标侧独有提交"，
  批错了要在**兑现之前**收回；而且已经放行、但因别的原因还没推成功时，授权也不该继续挂着。
  此前没有撤销口子 —— 唯一的"取消"办法是等它被用掉，而那正是最不该覆盖的时刻。
  修法：放行记录加 `revoked_at` / `revoked_by`；`IsApproved` 同时排除已撤销与已用掉；
  新增 `ForcePushStore.Revoke(id, actor)`。记录保留（谁批的、谁撤的、什么时候）可查。
- **backup_on_demand 兑现不了时说清楚为什么**（`executor`）：该策略要求覆盖前先打回滚快照，
  但没配 `sync.backup_dir` 就打不了。此前静默按 `block` 处理 ⇒ 策略与实际行为不符
  （操作者以为配了"先快照再覆盖"，实际被拦下），且看不出原因，像是工具随机拦的。
  修法：错误文案点名 `sync.backup_dir` 并说明"本次按 block 处理"。

### Changed

- go-git-platform 抬到 **v0.97.0**：gogit 后端的 http/https 传输加了
  **拨号超时 30s / 响应头超时 60s / TLS 握手超时 15s**。
  此前 go-git 的默认传输（`http.DefaultTransport`）**没有任何超时** ⇒ 上游"连不上/
  连上不回"时 git 操作会一直挂着，调用方只能等自己 context 的超时兜底
  （实测：GitHub 间歇性不可达时一次克隆卡到操作系统默认的 TCP 重试超时，约 2 分钟）。

## [v0.14.0] - 2026-10-24

### Fixed

- **强推放行改为一次性**（`executor` / `service` / `dao` / `model`）：契约写的是
  「Admin **一次性**放行后允许**本次** force」，但实现只查 `approved` 标志 ⇒
  一次人工放行变成该任务+分支上的**常驻后门**，之后每次覆盖目标侧独有提交都无人复核。
  修法：放行记录加 `consumed_at`；分歧检查把「用掉了这次放行」如实报给调用方，
  推送**成功后**由调用方消费掉。下次同任务同分支再出现分歧，必须重新审批。
  （`ForcePushApprover` 接口新增 `ConsumeForcePushApproval` —— 外部实现方需同步。）
- **失败时不再一律删掉工作区**（`executor`）：失败即删 ⇒ 下一次执行被迫全量克隆，
  「增量 fetch」（大仓的主要成本）收益在第一次失败后归零；而失败常与本地状态无关
  （推送被分歧保护拦下、鉴权失败、网络抖动）。修法：工作区**可用**就保留（下次增量 fetch），
  **坏了**（克隆没成功/被中断，HEAD 解析不出来）才删。
- **漂移检测的判词与计数**（`service/lifecycle_service.go`）：此前只要两端 ref 不等
  就一律报 `diverged`，且 ahead/behind 用 `rev-list` 算 —— 远端提交没 fetch 进本地对象库时
  该命令直接失败，静默给出 0/0，于是「漂移了多少」变成「不知道」却看着像「没有」。
  修法：区分 `diverged` / `local ahead` / `remote ahead`；算不出数量时明说
  「ahead/behind unknown」而不是给 0/0。

### Changed

- `ErrBackupDisabled` 导出（原 `errBackupDisabled`）：壳层据此把「冷备未启用」映射成
  可读提示，而不是笼统的 500。
- `VerifyBackupManifest`：还没有冷备清单时返回可读结果（`ok:false` + 说明该先做冷备），
  不再是 500 `load manifest: no such file or directory`。

## [v0.13.0] - 2026-10-24

### Fixed

本轮在「GitHub ↔ 自建 Forgejo 双向同步」实测中定位并修掉的 core 侧缺陷。
前三条是**报成功但数据错**级别：任务状态显示成功，目标侧实际没前进 / 目标侧独有提交被覆盖。

- **增量同步永不前进**（`executor/fetchRepo`）：`git fetch` 只更新
  `refs/remotes/origin/<branch>`，**本地分支仍停在旧提交**；而 push 推的是
  `refs/heads/<branch>`（本地）。于是目标侧永不更新，git 回 `already up-to-date`
  被当成成功 —— **首次克隆正常，第二次起每次都空转**。
  修法：fetch 之后把本地分支对齐到源端 ref（HEAD 在该分支上用 `reset --hard`，
  否则用 `update-ref` 只动 ref，不牵动别的分支）。
- **分歧保护自我失效**（`executor/checkDivergencePolicy`）：检查目标分叉时 fetch
  **没带凭证**（自建实例/私有仓/过期证书连不上），而 fetch 失败被当作「无分歧风险」
  直接放行 ⇒ 目标侧独有提交被 force push 覆盖，**任务仍报成功**。
  修法：fetch 带上平台凭证；连不上时**fail-closed**（本地若已有该目标分支的远端跟踪
  ref，说明目标确实有这个分支，必须报错让人工介入）。
- **执行结束回写状态抹掉并发修改**（`service/task_service.go`）：回写用整行
  `Save(task)`，而 task 是**执行开始时**的快照 ⇒ 执行期间改的 `force_push_policy` /
  `cron` / 开关会被旧快照悄悄改回去（实测：一次 cron 执行把刚改的
  `force_push_policy=allow` 还原成 `block`，表现为「改了没生效」）。
  修法：回写只碰自己拥有的两列（`last_run_at` / `last_status`）。
- **cron 锁 TTL 与续期间隔冲突**（`service/concurrency.go`）：续期间隔是**锁级固定值**，
  而 cron 锁 TTL 曾等于续期间隔 ⇒ 第一次续期就撞上 key 过期，锁从未被续上；
  锁只在前 10 秒有效，之后多实例可并发跑同一任务。
  修法：cron 锁 TTL 提到 `3×renewInterval`，**并在本次执行结束后显式释放**
  （不释放 = 续期成永久锁 ⇒ 之后每次 tick 都拿不到，cron 静默停摆）。
- **已设的 cron 清不掉**（`model.UpdateTaskRequest.Cron`）：曾是普通 string，空串与
  「没传」不可区分 ⇒ 一旦设了 cron 就再也改不掉（只能删任务重建）。
  修法：改 `*string`（nil=不修改 / 空串=清空）。
- **`git_tags=true` 从来没把 tag 推到目标端**（`executor/push`）：主推送只带分支
  refspec，tag 只在 fetch / bundle / backup-remote 里被用到。修法：tag 单独推
  （不覆盖已存在的 tag，等同 `git push --tags`）；批量失败时退化成**逐个推** ——
  git 的批量 push 是全有全无，一个冲突的 tag 会把其它本来没问题的 tag 一起挡住。
- **分歧错误文案里的「独有提交数」是错的**（`executor/divergentError`）：用的是
  **有上限截断的展示列表长度**（maxDivergentLog=5），目标侧只有 1 个独有提交时文案写
  「has 5 commit(s)」。修法：改用 rev-list 的真实 behind 计数。
- **`GetAPIURL` 把 github.com 拼成 `github.com/api/v3`**（`model/platform.go`）：那是
  **GitHub Enterprise** 的路径，github.com 的 REST API 在 `api.github.com`
  ⇒ 按旧逻辑建出来的 GitHub 平台必坏。修法：已知 SaaS 实例用整域名基址表
  （与 SDK 的 `knownHostPlatforms` 对齐），并加交叉校验测试。
- **glob 多分支两处非法 refspec**（`executor`）：`git clone --branch 'release/*'` 与
  `git fetch origin 'release/*'` 都不是合法用法 ⇒ 多分支任务从来跑不起来。
  修法：多分支走全量 clone / 全量 fetch，再按 glob 建出本地分支。
- **镜像引擎增量 fetch 不更新工作区**（`service/mirror_service.go`）：镜像发布从
  **工作区**取材，fetch 后工作区还是旧的 ⇒ 每次发旧快照。修法：fetch 之后把工作区
  对齐到源端默认分支。
- **镜像发布没把平台 TLS 设置传给 go-git 传输**（`mirror` + `service`）：
  `TransportAuth()` 只产出 `transport.AuthMethod`，不带 TLS 开关 ⇒ 自建实例
  （自签/过期证书）的「远端一致性检查」必然失败、发布直接中止。修法：显式传
  `InsecureSkipTLS`，并按 host 找回目标所属平台补上它的 TLS 策略。

### Added

- **历史同步的端到端门禁测试**（`service/history_e2e_test.go`）：core 层跑完整条流水线
  —— 建通道 → 配目标 → 裁决 → 预检 → 移植 → 推送 → 公开仓真的前进 → 审计链完整 →
  回滚 → 再推。含**冲突裁决路径**（内容级断言：产物里确实是裁决后的内容）与
  **source_branch 路径**（发布内容来自指定分支而非 HEAD）。
- **门禁守门人**（`service/history_gate_guard_test.go`）：编译不过的树不许推出去
  （apply 状态 failed + 推送被拒 + 远端拿不到坏文件）；产物在 apply 之后被改动，
  推送被拒。
- **身份改写的残留与边界测试**（`mirror/residual_test.go`、`mirror/rewrite_git_test.go`、
  `mirror/gate_test.go`）：二进制/超限文件里的内网标识 ⇒ 中止发布；symlink 目标过改写；
  克隆 URL（`.git` 后缀）与 scp 形式克隆地址整体改写；`Rewriter.Pending` 判据。
- **DAO 层索引回归测试**（`dao/history_dao_index_test.go`）：待推送产物清单必须走索引
  （`EXPLAIN QUERY PLAN` 钉住，不许退回 `SCAN history_runs`），语义与旧实现逐条对齐。
- **锁的续期守门人**（`lock/renew_test.go`）：钉住 `oekit/redisx` issue #33 的修复 ——
  `Unlock` 后不留孤儿续期 goroutine、不打「锁丢了」的 WARN（真 Redis，不可用时 skip）。

- **镜像发布支持痕迹清理**（`mirror.Options.ScrubRules` + `model.MirrorChannel.ScrubRules`）：
  身份改写只管 **module path**，而内容里还有别的内网标识 —— 内网 registry 地址、
  部署代号、令牌。它们原样进**公开 module** 就是泄漏（公开 module 任何人能 `go get`）。
  历史同步侧早就有这套规则，镜像侧没有 —— 同一个仓、两条发布链路、两套公开化口径。
  - 痕迹原语从 `mirror/history` **上提到 `mirror` 包**（`mirror/scrub.go`），
    `history.ScrubRule` 等改为**类型别名** ⇒ 上层（model / service / 报告）零改动，
    且不存在两份实现。改写顺序与历史同步侧一致：rewrite → gofmt → scrub。
  - `auto` 规则按 Replacement 替换（为空则删除命中内容）；
    `mandatory`（密钥类）**中止发布** —— 留痕不等于可以发布，密钥进公开 module 就是泄漏
    （历史同步侧是「该文件不发布」，镜像发布是整棵树，只能整棵中止）。
  - 报告里带 `ScrubHits`（密钥类不回显原文，只标记位置）。
- **镜像目标支持 SSH 主机密钥钉扎**（`insecureSkipTLS` / `hostKeyFingerprint` /
  `knownHostsPath`）：目标侧之前**没法钉** —— 于是同一次发布里
  「检查路径」无 known_hosts 就硬错（fail-closed）、「推送路径」却用 accept-new（TOFU），
  **两套策略相反**。现在两个路径都从同一份 `AuthConfig` 走，配了指纹就都校验。
  钉扎信息**落库**（丢了等于没钉：重启后悄悄退化成不校验）。
- **痕迹清理的发布侧守门人**（`mirror/scrub_publish_test.go`）：auto 规则清掉内网标识；
  mandatory（密钥类）命中 ⇒ 中止发布且远端不留任何引用。

### Changed

- **一键同步（HistorySync）只跑一遍流水线**：plan/resolve 一次、物化一次、门禁一次。
  旧实现是「先 `HistoryPreview`（临时目录物化 + 门禁）再 `applyRun`（产物目录物化 + 门禁）」
  —— 实仓上 plan+resolve≈65s、物化≈50s、类型门禁≈60s，白跑一遍（一键同步时间翻倍）。
  更要紧的是**一致性**：两遍算出的 head 可能不同（两次计算之间仓变了），
  预检报告说的就不是真正要推的那棵树。现在预检报告与执行记录**出自同一份物化结果**。
  - 新增 `planAndResolve` / `materialize` / `previewFrom` 三个共享步骤，
    独立调用的 `preview` 与 `apply` 各自算一遍（两次调用之间仓可能变了，必须重算）。
  - 新增 `HistoryService.MaterializeRuns()`（物化次数可观测性指标）。
- **「发布形态」单一实现**（`mirror/rewrite.go` 的 `Rewriter.PublishForm` / `GofmtNormalize`）：
  = 身份改写 + gofmt 归一化。镜像发布侧此前**只改写、不归一化**，历史同步侧改写后还要 gofmt
  ⇒ 同一份源码经两条链路发布出**不同的字节**（import 顺序因 module path 改写而翻转）
  ⇒ 公开仓的 gofmt CI 挂、`go get` 用户拿到未格式化代码，而**类型门禁看不出来**
  （编译不看 import 顺序）。`mirror/history` 的 `normalizeSource` 已删除，改调公共实现。
- **镜像发布：覆盖与盲推拆成两个开关**（`mirror.Options`）：
  - `AllowOverwrite`：**只**允许覆盖 tree 不一致的同名 tag；
  - `AllowUnverifiedPush`：远端不可达/认证失败时盲推（**新开关，默认关**）。
  原实现里 `AllowOverwrite` 顺带放行了盲推（文档没写）——
  一个开关同时管「覆盖已发布版本」与「查不到远端状态」，等于把两个风险捆在一起授权。
- **镜像发布：tag 与 main 拆成两次推**（`mirror/mirror.go`）：一次 push 带两个 refspec 时，
  公开仓保护默认分支会拒掉 main 那个 ref，而没有 `--atomic` 时
  **tag 其实已经推上去了**，core 却返回失败、壳层把它记成「发布失败」——
  报告与事实相反。现在先推 tag（对外可见的 module 版本），再 best-effort 推 main
  （失败只记警告，不影响发布结论）。
- **镜像发布：内容一致就不重推 tag**（`mirror/mirror.go`）：幂等重跑不该改写
  对外可见的 tag 对象 —— tag 的 hash 变了，**已经 pin 了 pseudo-version**
  （形如 `v0.0.0-<时间>-<commit12>`）的消费者就拉不到那个版本了。
  触发场景：源仓「重打」同名 tag（内容不变、提交元数据变了）⇒ 本次合成的
  commit 与远端已有的不同、但 tree 相同。现在这种情形**什么都不动**，
  报告里用 `TagAlreadyPublished` 标注。
- **镜像发布：main 快照不被补历史回退**：main 是公开默认分支
  （`git clone` 公开仓 / `go get module@main` 拿的是它），必须始终指向最新发布的版本。
  原实现是「main ← 本次发布的 tag」⇒ 先发 v1.0.0、再补发 v0.9.0 会把 main 回退到 v0.9.0。
  现在只有本次 tag 比远端已有版本更新时才更新 main（`mirror/version.go` 的版本比较）。

- **历史同步的门禁落在产物上**：`applyRun` 对**产物目录**跑真实门禁（gofmt + 全量类型
  检查），结论以结构化形式写进执行记录（`gate_report`：`ok` / `head` / 失败细节）。
  门禁不通过时 apply 状态为 `failed`（如实反映「这棵树推出去编译不过」）。
- **推送前重新核对「门禁结论 + 产物路径 + 产物 HEAD」三者一致**（`verifyPushableProduct`）：
  一致 ⇒ 门禁验过的就是即将推出去的那棵树；任一不一致 ⇒ 中止、不推送。
  **该闸门不可绕过**：`Force`（忽略阻塞项）与 `ForcePush`（覆盖远端）都不越过它。
- **门禁错误的内网标识与本机路径脱敏**：错误文本会流进模型上下文与日志，而通道 DTO
  不暴露 `module_source` / `host_source` —— 错误文本就成了内网标识外流的口子。
  内网标识（含 module path 去掉主机段的部分）替换为 `[内网标识已脱敏]`；
  本机绝对路径（用户主目录、Go module 缓存）替换为 `[本机路径已脱敏]`。
  定位信息（`file:line:col`）原样保留 —— 那是诊断的关键。
- **module 规则额外编译 scp 变体**：`git@host:org/repo.git` 的路径分隔符是 `:`，
  module 规则按 `/` 匹配压根碰不到它 ⇒ 内网组织名与仓库名原样进公开仓。
  变体把源/目标的**第一个**分隔符换成 `:`（报告里不单列，执行时照常生效）。
- **`history_runs` 补复合索引**（`model/history_channel.go`）：`(kind, status)` 与
  `(channel_id, kind)`。待推送产物清单这条查询挂在**每次 apply 与每次 push 的热路径上**，
  旧实现是全表扫描（1 万行时 `EXPLAIN` 给出 `SCAN history_runs`）。
- **`ListPendingProductDirs` 改成一条 SQL**（`NOT IN` 子查询 + `Pluck`）：
  旧实现全表捞整行、再在内存里做差集。
- **身份镜像的空仓判定改用 `errors.Is(err, transport.ErrEmptyRemoteRepository)`**：
  旧实现用 `strings.Contains(err.Error(), "empty")` —— 任何含 "empty" 的错误
  （`empty git-upload-pack given`、`cred session: empty secret`）都被当成空仓库，
  于是「一致性检查失败」被降级成「跳过」⇒ 可能覆盖已发布的同名 tag，而报告说「没覆盖」。
- **`mirror.TagReport` 补 `ResidualFiles`**：仍带着内网标识的文件清单（非空即中止发布）。
  `ReplacedFiles` 的语义收紧为「内容**真的**被改写」的文件。

- **痕迹清理原语上提到 `mirror` 包**（`mirror/scrub.go`）：`mirror/history` 侧改为类型别名
  （`history.ScrubRule` / `ScrubHit` / `ScrubResult` / `ScrubSummary` / `ApplyScrub` …），
  **调用方零改动**，但不再有两份实现。

### Fixed

- **远端判定统一到一处**：① `ensureRemote` 在 `service` 与 `mirror/history` 各有一份
  （URL 列表为空的处理还不一样）⇒ 导出 `history.EnsureRemote` 单一实现。
  ② `isEmptyRemoteErr`（服务侧）用字符串匹配三个子句，**真正的空仓一个都没命中**
  （实测：空仓 = `remote repository is empty` = `transport.ErrEmptyRemoteRepository`），
  于是首次发布到空远端时预检报「远端状态未知」；
  而 `"not found"` + `"refs/heads/"` 那一支反过来会把「远端不存在 / 认证失败」
  误判成「空仓库(首次发布),推送会创建该分支」。现在按 `errors.Is` 精确判空，
  与镜像发布侧同一套判定。

- **历史同步两步法（apply → push）此前根本不跑类型门禁**（第四轮实测）：全仓唯一的门禁
  调用点只有预检与身份镜像；apply 往执行记录里写的是一句占位文字，push 完全不看门禁 ——
  于是「源仓里有编译不过的包」照样被推到公开仓，而报告看上去一切正常。
  现在门禁跑在产物上、结论入记录、推送前核对（见上）。
- **产物仓的 HEAD 一直是游离（detached）状态**（`mirror/history/apply.go`）：
  go-git v5.19 的 `Checkout(Hash: ...)` 内部走 `setHEADToCommit`，而它**无条件把 HEAD
  写成游离的 hash 引用**。于是产物仓里提交不会移动分支、`git status` 显示
  "HEAD detached"、按 HEAD 取树的脚本取到的是游离态。apply 结束后恢复 HEAD 的符号引用。
- **身份镜像发布后公开仓仍留内网标识**（第四轮实测）：
  - `https://git.enjoye.top/enjoydream/app.git` 整条原样进公开仓（`.git` 被当成
    module path 的延续字符 ⇒ 边界不成立），且是「半改半留」；
  - >8MB 与含 NUL 的 blob 走「原样保留」路径时，内网标识原样保留，而报告写着
    「二进制被排除在改写之外」；
  - symlink 的目标从不改写。
  现在：`.git` 后缀特判 + scp 变体、symlink 目标过改写、残留一律**中止发布**并列出文件。
- **合并区域合成的方向性**（`mirror/history/merge.go`）：只在一侧插入、另一侧相同的情形下
  双向合成，修复「一侧新增被另一侧吸收后内容丢失」。

## [v0.12.0] - 2026-10-23

### Added

- **历史同步（双仓内容差异发布）**：新增 `mirror/history` 包——两个各自有完整历史的
  仓库之间按**内容差异**同步。与镜像中心的「身份镜像」并存，语义不同：
  那边是同一仓库的多个身份，这边是内网源树 → 公开目标树的内容移植。
  - `Plan` / `Diff`：生成同步计划（逐文件处置：带过去 / 排除 / 改写）。
  - 冲突规则（`ConflictRuleConfig`）：机械可判的冲突自动裁决。
  - `ScrubRule`：痕迹检测与清理（内网标识泄漏到公开树的防护），强度分
    `auto` / `mandatory`。
  - `Resolve`：为 hold 文件计算解法（`keep_target` / `take_source` / `merged`），
    并标出是否仍需人工介入。
  - `Dossiers`：未解决冲突的完整卷宗，可渲染成 Markdown 交 AI 或人审。
  - `Materialize` / `Verify`：把计划落到「预期目标树」+ 发布前门禁（gofmt 等）。
  - `Push`：按通道目标的顺序逐个推送、各自凭据，任一失败立即中止。
- **通道优先的历史同步 API**（`service/history_service.go` /
  `history_channel_service.go` / `history_preview.go`）：配置只在通道里，执行按
  channel id 进行，调用方不重传规则、也不回传服务端临时路径。DTO 与 Go 内部类型
  解耦（规则种类用字符串、痕迹模式用正则源码）。执行记录进哈希链。
- **历史同步的持久层与模型**：`dao/history_dao.go`、`model/history_channel.go`、
  `model/history_override.go`。

### Changed

- `typeCheckGate` → `TypeCheckGate`（由未导出转导出）。
- `github.com/sergi/go-diff` 由间接依赖升为直接依赖（内容差异计算要用）。

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
