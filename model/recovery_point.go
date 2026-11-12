package model

import "time"

// RecoveryPoint 恢复点索引(P3):一次成功冷备的可检索条目。
//
// BackupManifest 回答「这些文件有没有被篡改」(完整性);
// 恢复点回答「我要恢复到某任务的某个状态,该用哪个文件」(可恢复性)——
// 按任务/时间检索,拿到 bundle 文件名 + 源端 HEAD SHA,
// 对应评审验收「仓库的最新同步状态、备份覆盖情况和可恢复性
// 都可以独立验证」。
type RecoveryPoint struct {
	ID      uint   `json:"id" gorm:"primaryKey"`
	TaskKey string `json:"taskKey" gorm:"size:128;not null;index"`
	// FileName 冷备文件名(basename,与 manifest Entry.Name 同口径):
	// 存文件名而非绝对路径——backup_dir 配置迁移后仍可按当前配置定位;
	// 文件被轮转删除后由查询侧惰性清理。
	FileName string `json:"fileName" gorm:"size:255;not null"`
	// Format bundle | zip(与 sync.backup_format 一致)。
	Format string `json:"format" gorm:"size:10"`
	// SourceSHA 冷备时源分支 HEAD(RevParse)——恢复点的「状态锚」。
	SourceSHA string `json:"sourceSha" gorm:"size:64"`
	// SizeBytes 冷备文件大小(记录时点)。
	SizeBytes int64     `json:"sizeBytes"`
	CreatedAt time.Time `json:"createdAt" gorm:"index"`
}

func (RecoveryPoint) TableName() string { return TableRecoveryPoints }
