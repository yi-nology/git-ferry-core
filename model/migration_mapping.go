package model

import "time"

// 迁移对象类型(见 MigrationMapping)。
const (
	MigrationObjectIssue = "issue"
	MigrationObjectPR    = "pull_request"
)

// 映射状态。
const (
	// MigrationMappingPending 已登记、目标对象创建结果未知(先写 pending
	// 再创建,进程在创建与回填之间崩溃时用于断点识别,不重复创建)。
	MigrationMappingPending = "pending"
	// MigrationMappingMigrated 目标对象已创建并回填编号。
	MigrationMappingMigrated = "migrated"
	// MigrationMappingConverged 已按映射收敛(状态/标签已对齐)。
	MigrationMappingConverged = "converged"
	// MigrationMappingFailed 创建失败(可重试:下次迁移重新走创建路径)。
	MigrationMappingFailed = "failed"
)

// MigrationMapping 源对象 → 目标对象的持久化身份映射(#41)。
//
// **为什么不能以标题为身份键**:GitHub/GitLab 都允许同仓库存在多个
// 同标题 Issue —— 标题碰撞会把 N 个源 Issue 归并到目标侧同一个对象,
// 内容/状态互相覆盖;标题可被修改、目标侧可能手工建过同标题 Issue,
// 也无法据此断点续迁。
//
// 身份键 = (source_platform, source_repo, object_type, source_number),
// 唯一索引保证同一源对象只有一条映射;目标侧编号回填后即可:
//   - 重复执行按映射增量收敛(不再猜标题);
//   - 中断后断点续迁(pending 映射显式列出,绝不静默再建一份);
//   - 后续增量迁移可按 source_updated_at / 指纹定位。
//
// 标题只是展示字段,绝不参与身份判定。
type MigrationMapping struct {
	ID uint `json:"id" gorm:"primaryKey"`
	// 源对象身份(唯一键四元组)。
	SourcePlatform string `json:"sourcePlatform" gorm:"size:32;not null;uniqueIndex:uk_migration_mappings_source,priority:1"`
	SourceRepo     string `json:"sourceRepo" gorm:"size:255;not null;uniqueIndex:uk_migration_mappings_source,priority:2"`
	ObjectType     string `json:"objectType" gorm:"size:32;not null;uniqueIndex:uk_migration_mappings_source,priority:3"`
	SourceNumber   string `json:"sourceNumber" gorm:"size:64;not null;uniqueIndex:uk_migration_mappings_source,priority:4"`
	// 目标对象坐标(创建成功后回填)。
	TargetPlatform string `json:"targetPlatform" gorm:"size:32"`
	TargetRepo     string `json:"targetRepo" gorm:"size:255"`
	TargetNumber   string `json:"targetNumber" gorm:"size:64;index"`
	// SourceUpdatedAt 最近一次迁移时源对象的更新时间(增量迁移水位)。
	SourceUpdatedAt time.Time `json:"sourceUpdatedAt"`
	// ContentFingerprint 源内容指纹(title/body/state/labels),用于判断
	// 是否需要再次收敛,以及在目标侧被改动时提供审计线索。
	ContentFingerprint string `json:"contentFingerprint" gorm:"size:64"`
	// Status ∈ MigrationMapping* 常量。
	Status string `json:"status" gorm:"size:20;not null;index"`
	// LastError 最近一次失败原因(观测/断点续迁诊断)。
	LastError  string     `json:"lastError" gorm:"type:text"`
	MigratedAt *time.Time `json:"migratedAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

func (MigrationMapping) TableName() string { return TableMigrationMappings }
