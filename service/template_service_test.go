package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTplMatchFilter 自壳 ops_service_test.go 的 TestMatchToFilter 迁移。
func TestTplMatchFilter(t *testing.T) {
	f := tplMatchFilter(map[string][]string{
		"include_globs": {"team-*"},
		"exclude":       {"team-x"},
	})
	require.NotNil(t, f)
	assert.True(t, f.Allow("team-a", "A"))
	assert.False(t, f.Allow("team-x", "X"))
}

func TestTplMatchFilter_Nil(t *testing.T) {
	f := tplMatchFilter(nil)
	require.NotNil(t, f)
	assert.True(t, f.Allow("any", "any"))
}
