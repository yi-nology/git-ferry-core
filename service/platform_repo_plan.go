package service

import (
	"log/slog"

	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

// repoUpsertPlan 平台仓库导入的增改计划(纯函数产物,便于单测)。
type repoUpsertPlan struct {
	ToCreate []*model.Repo
	ToUpdate []*model.Repo
}

// planRepoUpsert 比对平台仓库与本地存量,产出创建/更新计划。
// 职责单一:只做 diff,不碰 DB。
func planRepoUpsert(platform *model.Platform, repos []*sdkprov.PlatformRepo,
	existingMap map[string]*model.Repo, filter *RepoImportFilter) *repoUpsertPlan {
	plan := &repoUpsertPlan{}
	for _, repo := range repos {
		if !filter.Allow(repo) {
			continue
		}
		cloneURL := rewriteCloneHost(repo.CloneURL, platform)
		sshURL := rewriteCloneHost(repo.SSHURL, platform)
		owner, pathName := splitRepoPath(repo.FullName, repo.Owner, repo.Name)

		existing := existingMap[repo.FullName]
		if existing == nil && repo.FullName == "" {
			existing = existingMap[owner+"/"+pathName]
		}
		if existing != nil {
			if needsMetaUpdate(existing, repo, owner, pathName, cloneURL, sshURL) {
				applyMetaUpdate(existing, repo, owner, pathName, cloneURL, sshURL)
				plan.ToUpdate = append(plan.ToUpdate, existing)
			}
			continue
		}

		key := repo.FullName
		if key == "" {
			key = owner + "/" + pathName
		}
		name := repo.Name
		if name == "" {
			name = pathName
		}
		plan.ToCreate = append(plan.ToCreate, &model.Repo{
			Key:           key,
			Name:          name,
			PlatformID:    platform.ID,
			Platform:      platform.Type,
			PlatformOwner: owner,
			PlatformRepo:  pathName,
			CloneURL:      cloneURL,
			SSHURL:        sshURL,
			DefaultBranch: repo.DefaultBranch,
			Status:        "active",
		})
	}
	return plan
}

func needsMetaUpdate(existing *model.Repo, repo *sdkprov.PlatformRepo,
	owner, pathName, cloneURL, sshURL string) bool {
	return existing.CloneURL != cloneURL ||
		existing.SSHURL != sshURL ||
		existing.PlatformOwner != owner ||
		existing.PlatformRepo != pathName ||
		(repo.Name != "" && existing.Name != repo.Name) ||
		(repo.DefaultBranch != "" && existing.DefaultBranch != repo.DefaultBranch)
}

func applyMetaUpdate(existing *model.Repo, repo *sdkprov.PlatformRepo,
	owner, pathName, cloneURL, sshURL string) {
	existing.CloneURL = cloneURL
	existing.SSHURL = sshURL
	existing.PlatformOwner = owner
	existing.PlatformRepo = pathName
	if repo.Name != "" {
		existing.Name = repo.Name
	}
	if repo.DefaultBranch != "" {
		existing.DefaultBranch = repo.DefaultBranch
	}
}

// persistRepoPlan 批量落库;批量失败回退逐条,尽量不丢数据。
func (s *PlatformService) persistRepoPlan(plan *repoUpsertPlan) int {
	count := 0
	if len(plan.ToUpdate) > 0 {
		if err := s.repoDAO.BatchUpdateRepoMeta(plan.ToUpdate); err != nil {
			slog.Error("sync repo: batch update repo meta failed", "error", err, "count", len(plan.ToUpdate))
		} else {
			count += len(plan.ToUpdate)
		}
	}
	if len(plan.ToCreate) > 0 {
		if err := s.repoDAO.BatchCreate(plan.ToCreate, 100); err != nil {
			slog.Error("sync repo: batch create failed", "error", err, "count", len(plan.ToCreate))
			for _, r := range plan.ToCreate {
				if err := s.repoDAO.Create(r); err != nil {
					slog.Error("sync repo: single create failed", "repo", r.Key, "error", err)
				} else {
					count++
				}
			}
		} else {
			count += len(plan.ToCreate)
		}
	}
	return count
}

// indexExistingByKey 以 FullName(Key) 建索引;展示名跨群组会撞车,不能当主键。
func indexExistingByKey(repos []*model.Repo) map[string]*model.Repo {
	m := make(map[string]*model.Repo, len(repos))
	for _, r := range repos {
		m[r.Key] = r
	}
	return m
}
