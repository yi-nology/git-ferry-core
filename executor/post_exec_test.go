package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestRunPostExec_HangingScriptIsKilled 挂住的 post_exec 脚本必须被超时终止。
//
// 回归背景(P2):post_exec 跑在收尾的 defer 里,原先用 exec.Command(无 ctx、无超时)
// ⇒ 脚本挂住(等输入/等网络)时 CompleteRunWithTaskUpdate 永不执行,
// run 记录永远停在 running,调用方持有的同 taskKey 互斥锁与全局并发槽一起被占住;
// 挂够 max_concurrent 次之后,整个服务的同步全部起不来。
func TestRunPostExec_HangingScriptIsKilled(t *testing.T) {
	old := postExecTimeout
	postExecTimeout = 300 * time.Millisecond
	defer func() { postExecTimeout = old }()

	script := filepath.Join(t.TempDir(), "hang.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nsleep 60\n"), 0o700))

	svc := newMockService()
	svc.config.Sync.PostExecScript = script
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	var details strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.runPostExec(&model.SyncTask{Key: "t1"},
			&model.SyncRun{Status: model.StatusSuccess}, &details)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("post_exec 挂住没被终止:收尾阶段被无限期拖住")
	}
	assert.Contains(t, details.String(), "超时", "details 里要说明是超时终止的")
}
