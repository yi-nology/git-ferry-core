package model

import (
	"time"

	"gorm.io/gorm"
)

// dbTimePrecision 是审计链哈希所覆盖时间的精度(毫秒)。
//
// **这个精度必须与数据库能原样存回的精度对齐**。
// 哈希是写入前算的,校验时是从库里读回来重算的 ——
// 两边参与计算的必须是同一个值,否则整条链都会被报「被篡改」。
//
// 实际踩过的坑(MySQL):GORM 的 MySQL 驱动默认 datetime(3),只存毫秒。
// 纳秒时间写进去会被 MySQL **四舍五入**,读回来就不是同一个值了,
// 于是每一条记录都误报「被篡改」(SQLite 单测全绿,换 MySQL 就全红)。
//
// 修法:写入前把时间截到毫秒。这样任何精度 ≥ 毫秒的库
// (MySQL datetime(3) / SQLite / PostgreSQL)都能原样存回。
const dbTimePrecision = time.Millisecond

// TruncateDBTime 把时间截到数据库能原样存回的精度(毫秒)。
//
// 先取 UTC 再截断:Truncate 按绝对时刻取整,顺序不影响结果,
// 但统一成 UTC 让存进去的值与哈希里的字符串一一对应。
func TruncateDBTime(t time.Time) time.Time {
	return t.UTC().Truncate(dbTimePrecision)
}

// TruncateDBTimePtr 是 TruncateDBTime 的指针版本(nil 安全)。
func TruncateDBTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := TruncateDBTime(*t)
	return &v
}

// hashTimeLayout 是哈希载荷里时间的固定格式(毫秒精度)。
//
// 固定宽度(而不是 RFC3339Nano 的「去掉尾部零」):同一个时刻必须
// 永远格式化成同一个字符串,否则哈希不可复算。
const hashTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// HashTimeString 把时间格式化成哈希载荷里的字符串。
//
// **先截到毫秒再格式化**:与 TruncateDBTime 同一个理由 ——
// 哈希必须在「写库前」与「读库后」算出同一个值。
func HashTimeString(t time.Time) string {
	return TruncateDBTime(t).Format(hashTimeLayout)
}

// HashTimeStringPtr 是 HashTimeString 的指针版本(nil → 空串)。
func HashTimeStringPtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return HashTimeString(*t)
}

// nowFunc 取数据库层的时间源(测试可替换)。
func nowFunc(tx *gorm.DB) time.Time {
	if tx != nil && tx.Config != nil && tx.Config.NowFunc != nil {
		return tx.Config.NowFunc()
	}
	return time.Now()
}
