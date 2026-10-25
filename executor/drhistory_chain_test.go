package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVerifyDrillChain_DetectsTruncationAndBrokenLines 演练链校验必须能查出
// **尾部截断**与**坏行**。
//
// 回归背景(P2):链只能证明"前后接得上",没有链尾锚点 ⇒
// ① 写两条后删掉最后一条 → 剩下的前缀仍是合法链 → 校验报 ok=true;
// ② 把一行改成非法 JSON → 解析失败被当噪声跳过 → 那条记录完全不可见。
//
// 历史同步链有 LastEntryHash 锚点专门检出截断,演练链没有 ⇒ 这里补上。
//
// 这条用例是**反向验证**:去掉锚点检查/把坏行重新当噪声跳过,本用例变红。
func TestVerifyDrillChain_DetectsTruncationAndBrokenLines(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "drill-history.jsonl")

	rep1 := &DrillReport{BundleName: "drill-1", Success: true}
	rep2 := &DrillReport{BundleName: "drill-2", Success: true}
	_, err := SaveDrillReport(file, rep1)
	require.NoError(t, err)
	_, err = SaveDrillReport(file, rep2)
	require.NoError(t, err)

	ok, checked, brokenAt := VerifyDrillChain(file)
	require.True(t, ok, "正常链必须校验通过: %s", brokenAt)
	assert.Equal(t, 2, checked)

	// --- 尾部截断:删掉最后一条 ---
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	lines := splitNonEmptyLines(string(data))
	require.Len(t, lines, 2)
	require.NoError(t, os.WriteFile(file, []byte(lines[0]+"\n"), 0o640))

	ok, _, brokenAt = VerifyDrillChain(file)
	assert.False(t, ok, "截断后必须检出(链尾锚点不匹配)")
	assert.Contains(t, brokenAt, "截断", "错误信息要说明是截断")

	// --- 坏行:把一行改成非法 JSON ---
	require.NoError(t, os.WriteFile(file, []byte("{not json}\n"+lines[1]+"\n"), 0o640))
	ok, _, brokenAt = VerifyDrillChain(file)
	assert.False(t, ok, "坏行必须计入 broken(不能当噪声跳过)")
	assert.Contains(t, brokenAt, "无法解析", "错误信息要说明是解析失败")
}

// splitNonEmptyLines 拆出非空行。
func splitNonEmptyLines(s string) []string {
	var out []string
	for _, l := range splitLines(s) {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	out = append(out, cur)
	return out
}
