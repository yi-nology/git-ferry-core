package service

import (
	"fmt"
	"strings"
)

// OrgMapStrategy 批量镜像时目标仓落点策略（对标 gitea-mirror）。
type OrgMapStrategy string

const (
	// OrgMapPreserve 保持源 owner/repo 结构。
	OrgMapPreserve OrgMapStrategy = "preserve"
	// OrgMapSingle 全部落到同一 org，仓库名不变。
	OrgMapSingle OrgMapStrategy = "single"
	// OrgMapFlat 全部落到目标 user，仓库名不变。
	OrgMapFlat OrgMapStrategy = "flat"
	// OrgMapMixed 个人仓→flat；组织仓→见 OrgMapOptions.MixedOrgToTarget。
	OrgMapMixed OrgMapStrategy = "mixed"
)

// ParseStrategy 解析策略名；空串视为 preserve。
func ParseStrategy(s string) (OrgMapStrategy, error) {
	switch OrgMapStrategy(strings.ToLower(strings.TrimSpace(s))) {
	case "", OrgMapPreserve:
		return OrgMapPreserve, nil
	case OrgMapSingle:
		return OrgMapSingle, nil
	case OrgMapFlat:
		return OrgMapFlat, nil
	case OrgMapMixed:
		return OrgMapMixed, nil
	default:
		return "", fmt.Errorf("unknown org_mapping %q (preserve|single|flat|mixed)", s)
	}
}

// ValidOrgMapStrategy 校验策略。
func ValidOrgMapStrategy(s string) bool {
	_, err := ParseStrategy(s)
	return err == nil
}

// OrgMapOptions 目标 owner/repo 落点选项。
//
// 各调用方语义不同，用显式开关表达，core 不做隐式改写：
//
//	导入（org_import）：RequireTarget=true, MixedOrgToTarget=false, PreserveRedirect=false
//	组织镜像（org_mirror）：MixedOrgToTarget=true, PreserveRedirect=false
//	落点缺失时 RequireTarget=true 报错，false 静默回落源 owner。
type OrgMapOptions struct {
	// Strategy 策略；空视为 preserve。
	Strategy OrgMapStrategy
	// TargetOrg single 落点；MixedOrgToTarget 时作 mixed 组织仓落点。
	TargetOrg string
	// TargetUser flat 与 mixed 个人仓落点。
	TargetUser string
	// SourceIsPersonal 源为个人命名空间（mixed 个人仓判定）。
	// 组织镜像侧可用 IsPersonalOwner(owner, TargetUser, nil) 得到「owner 已等于目标用户」的等价判定。
	SourceIsPersonal bool
	// PreserveRedirect=true 时 preserve 分支按落点重定向：
	// 组织仓→TargetOrg、个人仓→TargetUser；false 保持源 owner。
	PreserveRedirect bool
	// MixedOrgToTarget=true 时 mixed 组织仓落 TargetOrg；false 保持源 owner。
	MixedOrgToTarget bool
	// RequireTarget=true 时 single/flat/mixed 个人仓缺落点返回错误；false 回落源 owner。
	RequireTarget bool
	// SourceKey owner/repo 为空时从此解析，支持 "platform/owner/repo" 或 "owner/repo"。
	SourceKey string
	// TargetPlatform 非空时拼 Result.Key。
	TargetPlatform string
}

// OrgMapResult 映射结果。
type OrgMapResult struct {
	Owner string
	Repo  string
	// Key 形如 <platform>/<owner>/<repo>，仅 TargetPlatform 非空时填充。
	Key string
}

// ResolveOrgTarget 计算目标 owner/repo。策略语义见 OrgMapOptions 注释。
func ResolveOrgTarget(sourceOwner, sourceRepo string, o OrgMapOptions) (OrgMapResult, error) {
	owner, repo := sourceOwner, sourceRepo
	if owner == "" || repo == "" {
		var err error
		owner, repo, err = splitOwnerRepo(o.SourceKey)
		if err != nil {
			return OrgMapResult{}, err
		}
	}
	if repo == "" {
		return OrgMapResult{}, fmt.Errorf("source repo is empty")
	}

	strategy := o.Strategy
	if strategy == "" {
		strategy = OrgMapPreserve
	}

	switch strategy {
	case OrgMapPreserve:
		if o.PreserveRedirect {
			if o.SourceIsPersonal {
				if o.TargetUser != "" {
					owner = o.TargetUser
				}
			} else if o.TargetOrg != "" {
				owner = o.TargetOrg
			}
		}
	case OrgMapSingle:
		if o.TargetOrg != "" {
			owner = o.TargetOrg
		} else if o.RequireTarget {
			return OrgMapResult{}, fmt.Errorf("org_mapping=single requires target_org")
		}
	case OrgMapFlat:
		if o.TargetUser != "" {
			owner = o.TargetUser
		} else if o.RequireTarget {
			return OrgMapResult{}, fmt.Errorf("org_mapping=flat requires target_user")
		}
	case OrgMapMixed:
		if o.SourceIsPersonal {
			if o.TargetUser != "" {
				owner = o.TargetUser
			} else if o.RequireTarget {
				return OrgMapResult{}, fmt.Errorf("org_mapping=mixed (personal) requires target_user")
			}
		} else if o.MixedOrgToTarget && o.TargetOrg != "" {
			owner = o.TargetOrg
		}
	default:
		return OrgMapResult{}, fmt.Errorf("unknown org_mapping %q (preserve|single|flat|mixed)", strategy)
	}

	out := OrgMapResult{Owner: owner, Repo: repo}
	if o.TargetPlatform != "" {
		out.Key = o.TargetPlatform + "/" + owner + "/" + repo
	}
	return out, nil
}

// splitOwnerRepo 解析 "platform/owner/repo" 或 "owner/repo"。
func splitOwnerRepo(key string) (string, string, error) {
	key = strings.Trim(strings.TrimSpace(key), "/")
	parts := strings.Split(key, "/")
	switch len(parts) {
	case 3:
		return parts[1], parts[2], nil
	case 2:
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("cannot parse owner/repo from %q", key)
	}
}

// IsPersonalOwner 判定源 owner 是否个人命名空间。
// orgOwners 为已知组织集合；owner 不在其中且等于 targetUser 时判为个人仓。
func IsPersonalOwner(owner, targetUser string, orgOwners map[string]bool) bool {
	if orgOwners[owner] {
		return false
	}
	return targetUser != "" && strings.EqualFold(owner, targetUser)
}
