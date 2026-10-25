package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestCreateRule_ExplicitDisabledAndZeroInterval 显式的"禁用/不限流"必须被 honored。
//
// 回归背景(P1):模型上 `Enabled bool gorm:"default:true"` / `MinInterval int
// gorm:"default:60"` ⇒ **GORM 会把零值字段替换成默认值** ⇒
// "先建一条禁用的规则、配好再启用"建出来就是 enabled=true ⇒ 规则立刻生效并
// 开始触发同步;"min_interval=0(不限流)"也被悄悄改成 60。
//
// 这条用例是**反向验证**:把模型上的 gorm default 标签加回去,本用例变红。
func TestCreateRule_ExplicitDisabledAndZeroInterval(t *testing.T) {
	svc, _ := setupWebhookTestService(t)

	rule, err := svc.CreateRule(context.Background(), &model.CreateRuleRequest{
		Name:          "先禁用再启用",
		RepoKey:       "test-repo",
		EventType:     "push",
		BranchPattern: "main",
		Action:        "sync",
		TaskKeys:      []string{"task-1"},
		MinInterval:   intPtr(0),
		Enabled:       boolPtr(false),
	})
	require.NoError(t, err)

	assert.False(t, rule.Enabled, "显式 enabled=false 必须被 honored(不得被 GORM 默认值吞掉)")
	assert.Equal(t, 0, rule.MinInterval, "显式 min_interval=0(不限流)必须被 honored")

	// 回读一次:确认真的落库了(不是只在内存对象上)
	stored, err := svc.GetRule(context.Background(), rule.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.False(t, stored.Enabled, "落库后仍必须是禁用的")
	assert.Equal(t, 0, stored.MinInterval, "落库后 min_interval 必须是 0")
}

// TestCreateRule_Defaults 没传时用默认值。
func TestCreateRule_Defaults(t *testing.T) {
	svc, _ := setupWebhookTestService(t)

	rule, err := svc.CreateRule(context.Background(), &model.CreateRuleRequest{
		Name:          "默认启用",
		RepoKey:       "test-repo",
		EventType:     "push",
		BranchPattern: "main",
		Action:        "sync",
		TaskKeys:      []string{"task-1"},
	})
	require.NoError(t, err)
	assert.True(t, rule.Enabled, "没传 enabled ⇒ 默认启用")
	assert.Equal(t, defaultRuleMinInterval, rule.MinInterval, "没传 min_interval ⇒ 用默认间隔")
}

// TestUpdateRule_PartialUpdate_KeepsOtherFields 局部更新不得改其它字段。
//
// 回归背景(P1):前端在规则列表里切换启用开关时提交的是
// `updateRule({id, enabled})` —— 只带这两个字段。旧实现无条件回写
// branchPattern / min_interval / description ⇒
// ① 分支模式被清空 ⇒ 规则从"只跟 main"悄悄放宽成"任何分支都触发";
// ② enabled 被写成 false(没带时)⇒ 规则被静默禁用 ⇒ 之后所有 webhook
// 都不再触发同步。
//
// 这条用例是**反向验证**:把 UpdateRule 改回无条件回写,本用例变红。
func TestUpdateRule_PartialUpdate_KeepsOtherFields(t *testing.T) {
	svc, _ := setupWebhookTestService(t)

	created, err := svc.CreateRule(context.Background(), &model.CreateRuleRequest{
		Name:          "只切启用开关",
		RepoKey:       "test-repo",
		EventType:     "push",
		BranchPattern: "main",
		Action:        "sync",
		TaskKeys:      []string{"task-1"},
		MinInterval:   intPtr(120),
		Enabled:       boolPtr(true),
		Description:   "只跟 main",
	})
	require.NoError(t, err)

	// 只带 id + enabled(= 前端列表页的开关)
	updated, err := svc.UpdateRule(context.Background(), &model.UpdateRuleRequest{
		ID:      created.ID,
		Enabled: boolPtr(false),
	})
	require.NoError(t, err)

	assert.False(t, updated.Enabled, "显式 enabled=false 必须生效")
	assert.Equal(t, "main", updated.BranchPattern, "分支模式不得被清空(否则规则悄悄放宽成全分支)")
	assert.Equal(t, 120, updated.MinInterval, "最小间隔不得被清零")
	assert.Equal(t, "只跟 main", updated.Description, "描述不得被清空")

	// 再切回来:分支模式仍要保持
	back, err := svc.UpdateRule(context.Background(), &model.UpdateRuleRequest{
		ID:      created.ID,
		Enabled: boolPtr(true),
	})
	require.NoError(t, err)
	assert.True(t, back.Enabled)
	assert.Equal(t, "main", back.BranchPattern)
	assert.Equal(t, 120, back.MinInterval)

	// 显式清空分支模式(= 匹配全部分支)是合法意图,必须能表达
	cleared, err := svc.UpdateRule(context.Background(), &model.UpdateRuleRequest{
		ID:            created.ID,
		BranchPattern: strPtr(""),
	})
	require.NoError(t, err)
	assert.Equal(t, "", cleared.BranchPattern, "显式清空分支模式必须生效")
	assert.Equal(t, 120, cleared.MinInterval, "清空分支模式时其它字段不受影响")
}
