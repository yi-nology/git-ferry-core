package repository

import (
	"strings"

	"gorm.io/gorm"
)

// EscapeLikePattern 转义 SQL LIKE 模式中的通配符(% 和 _),
// 防止用户搜索词被当作通配符使用导致意外宽匹配。
func EscapeLikePattern(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// Paginate 执行分页查询(本仓 Repo 契约是**直接 offset**,不是 page/pageSize)。
//
// 有意不换成 gormx.Paginate:pkg/pagination 的 PageRequest 只能由 Page/PageSize
// 派生 offset,offset 非页对齐时换算会改变语义;壳层 converter 已按 offset 换算。
// 待 API 契约改为 page 制的入口再收编。
func Paginate[T any](db *gorm.DB, page Pagination, dest *[]*T) (int64, error) {
	var total int64
	if err := db.Model(new(T)).Count(&total).Error; err != nil {
		return 0, err
	}
	err := db.Offset(page.Offset).Limit(page.Limit).Order("id DESC").Find(dest).Error
	return total, err
}
