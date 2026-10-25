package model

// ForcePushPolicy 强制推送保护策略。
//
// **唯一权威定义**:service 与 executor 共用同一份实现,避免两份平行逻辑漂移。
// 放在 model 包的原因:service import executor,executor 不能再反向依赖 service
// (import cycle),而两边都需要解析策略 ⇒ 权威实现必须落在两者共同的下游。
type ForcePushPolicy string

const (
	// ForcePushAllow 允许强制覆盖目标侧独有提交(不保护分歧)。
	ForcePushAllow ForcePushPolicy = "allow"
	// ForcePushBlock 遇分歧直接失败,不覆盖(可经人工审批放行)。
	ForcePushBlock ForcePushPolicy = "block"
	// ForcePushBackupOnDemand 覆盖前自动打 bundle 快照(可回滚)。
	ForcePushBackupOnDemand ForcePushPolicy = "backup_on_demand"
)

// ResolveForcePushPolicy 从任务配置解析生效策略。
//
// **空值 = 未显式配置 ⇒ block(最安全)**。
// 旧的 keep_divergent 布尔字段已删除(它只是本策略的旧拼写),不再参与解析:
// 两套字段并存会让调用方各选一个、且可能各说各话(前端一度按 keep_divergent
// 展示策略,与实际生效的 force_push_policy 不一致)。
//
// 未知/非法策略同样按 block 处理(最安全),不静默放行。
func ResolveForcePushPolicy(t *SyncTask) ForcePushPolicy {
	if t == nil {
		return ForcePushBlock
	}
	switch p := ForcePushPolicy(t.ForcePushPolicy); p {
	case ForcePushAllow, ForcePushBlock, ForcePushBackupOnDemand:
		return p
	}
	return ForcePushBlock
}

// ValidForcePushPolicy 校验策略字符串是否合法。
func ValidForcePushPolicy(p string) bool {
	switch ForcePushPolicy(p) {
	case ForcePushAllow, ForcePushBlock, ForcePushBackupOnDemand:
		return true
	}
	return false
}
