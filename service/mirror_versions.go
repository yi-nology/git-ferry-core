package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	"github.com/yi-nology/git-ferry-core/model"
)

// versionCell 单 (tag,target) 的执行结果单元。
type versionCell struct {
	state, executedAt, commit, tree, verify string
}

// loadTagCommits 从本地克隆读 tag→commit;失败仅告警,不阻断矩阵展示。
func (m *MirrorService) loadTagCommits(ctx context.Context, ch *model.MirrorChannel, channelID uint) map[string]string {
	tagCommits := map[string]string{}
	dir, cleanup, err := m.ensureRepo(ctx, ch)
	if err != nil {
		slog.Warn("mirror: 源仓库不可用,仅展示历史记录", "channel", channelID, "error", err)
		return tagCommits
	}
	defer cleanup()
	infos, listErr := m.backend.GetTagList(ctx, dir)
	if listErr != nil {
		slog.Warn("mirror: 列出 tag 失败", "channel", channelID, "error", listErr)
		return tagCommits
	}
	for _, ti := range infos {
		tagCommits[ti.Name] = ti.Hash
	}
	return tagCommits
}

// buildVersionCells 从执行历史归并 (tag,target) 状态;runs 按 id 倒序,首个即最新。
func buildVersionCells(runs []*model.MirrorRun) map[string]map[uint]*versionCell {
	cells := map[string]map[uint]*versionCell{}
	for _, run := range runs {
		var sts map[string]*model.TagStatus
		_ = json.Unmarshal([]byte(run.TagStatuses), &sts)
		for tag, st := range sts {
			if cells[tag] == nil {
				cells[tag] = map[uint]*versionCell{}
			}
			c := cells[tag][run.TargetID]
			if c == nil {
				c = &versionCell{state: "unpublished", verify: "unverified"}
				cells[tag][run.TargetID] = c
			}
			switch run.Kind {
			case model.MirrorKindVerify:
				if c.verify == "unverified" {
					if st.Status == model.MirrorRunSuccess {
						c.verify = "passed"
					} else {
						c.verify = "failed"
					}
				}
			default:
				c.state = st.Status
				c.executedAt = formatTime(run.StartedAt)
				c.commit, c.tree = st.Commit, st.Tree
			}
		}
	}
	return cells
}

// assembleVersionMatrix 合并 tag 来源(历史 ∪ 远端)并按目标展开。
func assembleVersionMatrix(mode string, targets []*model.MirrorTarget,
	tagCommits map[string]string, cells map[string]map[uint]*versionCell) []MirrorVersion {
	seen := map[string]bool{}
	var versions []MirrorVersion
	appendVersion := func(tag string) {
		if seen[tag] {
			return
		}
		seen[tag] = true
		v := MirrorVersion{Tag: tag, Commit: tagCommits[tag]}
		for _, t := range targets {
			c := cells[tag][t.ID]
			mvt := MirrorVersionTarget{
				TargetID: t.ID,
				Target:   t.TargetModule,
				State:    "unpublished",
				Verify:   "unverified",
			}
			if mode == model.MirrorModeBackup {
				mvt.State = "unbackedup"
			}
			if c != nil {
				mvt.State, mvt.ExecutedAt, mvt.Commit, mvt.Tree, mvt.Verify =
					c.state, c.executedAt, c.commit, c.tree, c.verify
			}
			v.Targets = append(v.Targets, mvt)
		}
		versions = append(versions, v)
	}
	for tag := range cells {
		appendVersion(tag)
	}
	for tag := range tagCommits {
		appendVersion(tag)
	}
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Tag > versions[j].Tag
	})
	return versions
}

// GetMirrorVersions 通道版本矩阵:tag × target 状态。
func (m *MirrorService) GetMirrorVersions(ctx context.Context, channelID uint) (*MirrorVersionsResult, error) {
	ch, err := m.channels.FindByID(channelID)
	if err != nil {
		return nil, errChannelNotFound(channelID)
	}
	targets, err := m.channels.FindTargets(channelID)
	if err != nil {
		return nil, err
	}
	tagCommits := m.loadTagCommits(ctx, ch, channelID)
	runs, err := m.runs.FindAllByChannel(channelID)
	if err != nil {
		return nil, err
	}
	cells := buildVersionCells(runs)
	versions := assembleVersionMatrix(ch.Mode, targets, tagCommits, cells)
	return &MirrorVersionsResult{Mode: ch.Mode, Module: ch.Module, Versions: versions}, nil
}

func errChannelNotFound(id uint) error {
	return fmt.Errorf("通道不存在: %d", id)
}
