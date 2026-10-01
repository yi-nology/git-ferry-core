package service

import "strings"

// OrgMapStrategy 批量镜像时目标仓落点策略（对标 gitea-mirror）。
type OrgMapStrategy string

const (
	// OrgMapPreserve 保持源 owner/repo 结构。
	OrgMapPreserve OrgMapStrategy = "preserve"
	// OrgMapSingle 全部落到同一 org，仓库名不变。
	OrgMapSingle OrgMapStrategy = "single"
	// OrgMapFlat 全部落到目标 user，仓库名不变。
	OrgMapFlat OrgMapStrategy = "flat"
	// OrgMapMixed 个人仓→flat；组织仓→preserve。
	OrgMapMixed OrgMapStrategy = "mixed"
)

// ValidOrgMapStrategy 校验策略。
func ValidOrgMapStrategy(s string) bool {
	switch OrgMapStrategy(s) {
	case OrgMapPreserve, OrgMapSingle, OrgMapFlat, OrgMapMixed, "":
		return true
	default:
		return false
	}
}

// ResolveOrgTarget 计算目标 owner/repo。
//
//	sourceOwner/sourceRepo 为源路径；targetOrg 用于 single（也可作 mixed 的组织落点）；
//	targetUser 用于 flat。preserve 时若 targetOrg 非空则整体替换 owner。
func ResolveOrgTarget(strategy OrgMapStrategy, sourceOwner, sourceRepo, targetOrg, targetUser string, sourceIsPersonal bool) (owner, repo string) {
	repo = sourceRepo
	switch strategy {
	case OrgMapSingle:
		if targetOrg != "" {
			return targetOrg, repo
		}
		return sourceOwner, repo
	case OrgMapFlat:
		if targetUser != "" {
			return targetUser, repo
		}
		return sourceOwner, repo
	case OrgMapMixed:
		if sourceIsPersonal {
			if targetUser != "" {
				return targetUser, repo
			}
			return sourceOwner, repo
		}
		if targetOrg != "" {
			return targetOrg, repo
		}
		return sourceOwner, repo
	default: // preserve / ""
		if targetOrg != "" && !sourceIsPersonal {
			return targetOrg, repo
		}
		if targetUser != "" && sourceIsPersonal {
			return targetUser, repo
		}
		return sourceOwner, repo
	}
}

// IsPersonalOwner 粗判：与 known org 集合无关时，调用方可传 org 列表。
// 默认把「等于 targetUser」或调用方标注的 personal 作为个人仓。
func IsPersonalOwner(owner, targetUser string, orgOwners map[string]bool) bool {
	if orgOwners[owner] {
		return false
	}
	if targetUser != "" && strings.EqualFold(owner, targetUser) {
		return true
	}
	return !orgOwners[owner] && targetUser != "" && strings.EqualFold(owner, targetUser)
}
