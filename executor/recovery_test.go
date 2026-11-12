package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// fakeRecoveryRecorder 捕获记录调用。
type fakeRecoveryRecorder struct {
	got  []string // "task|file|format|sha|size"
	fail bool
}

func (f *fakeRecoveryRecorder) RecordRecoveryPoint(taskKey, fileName, format, sourceSHA string, sizeBytes int64) error {
	if f.fail {
		return assert.AnError
	}
	f.got = append(f.got, taskKey+"|"+fileName+"|"+format+"|"+sourceSHA)
	return nil
}

// revParseBackend 只覆写 RevParse(内嵌零值接口,其它方法 panic=未预期调用)。
type revParseBackend struct {
	gitbackend.GitBackend
	sha string
	err error
}

func (b revParseBackend) RevParse(context.Context, string, string) (string, error) {
	return b.sha, b.err
}

// TestRecordRecoveryPoint 冷备成功后记恢复点:文件名+源 HEAD+格式。
func TestRecordRecoveryPoint(t *testing.T) {
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, RepoDir)
	require.NoError(t, os.MkdirAll(repoDir, 0o750))
	bundlePath := filepath.Join(workDir, "a.bundle")
	require.NoError(t, os.WriteFile(bundlePath, []byte("data"), 0o600))

	rec := &fakeRecoveryRecorder{}
	e := &Executor{
		backend:  revParseBackend{sha: "deadbeef"},
		Recovery: rec,
	}
	rc := &RunContext{
		Task:    &model.SyncTask{Key: "t-rec"},
		WorkDir: workDir,
		Details: &strings.Builder{},
		Exec:    e,
	}

	e.recordRecoveryPoint(rc, bundlePath, "")
	require.Len(t, rec.got, 1)
	assert.Contains(t, rec.got[0], "t-rec|a.bundle|bundle|deadbeef")

	// format 显式传 zip 时透传
	e.recordRecoveryPoint(rc, bundlePath, "zip")
	assert.Contains(t, rec.got[1], "|zip|")
}

// TestRecordRecoveryPoint_Degrades 三种降级:未接线/RevParse 失败/记录失败
// ——都不 panic,且不把错误升级成冷备失败(观测面不得毁主流程)。
func TestRecordRecoveryPoint_Degrades(t *testing.T) {
	workDir := t.TempDir()
	bundlePath := filepath.Join(workDir, "a.bundle")
	require.NoError(t, os.WriteFile(bundlePath, []byte("x"), 0o600))

	// Recovery=nil:静默跳过
	e := &Executor{backend: revParseBackend{sha: "s"}}
	rc := &RunContext{Task: &model.SyncTask{Key: "t"}, WorkDir: workDir, Details: &strings.Builder{}, Exec: e}
	assert.NotPanics(t, func() { e.recordRecoveryPoint(rc, bundlePath, "") })

	// RevParse 失败:不记录
	e2 := &Executor{backend: revParseBackend{err: assert.AnError}, Recovery: &fakeRecoveryRecorder{}}
	rc2 := &RunContext{Task: &model.SyncTask{Key: "t"}, WorkDir: workDir, Details: &strings.Builder{}, Exec: e2}
	e2.recordRecoveryPoint(rc2, bundlePath, "")
	assert.Contains(t, rc2.Details.String(), "rev-parse HEAD failed")

	// 记录失败:只留痕
	rec := &fakeRecoveryRecorder{fail: true}
	e3 := &Executor{backend: revParseBackend{sha: "s"}, Recovery: rec}
	rc3 := &RunContext{Task: &model.SyncTask{Key: "t"}, WorkDir: workDir, Details: &strings.Builder{}, Exec: e3}
	e3.recordRecoveryPoint(rc3, bundlePath, "")
	assert.Contains(t, rc3.Details.String(), "record failed")
}
