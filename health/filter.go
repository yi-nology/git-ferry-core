package health

import (
	"github.com/oekit/pkg/pathx"
)

// Filter 过滤链(借鉴 gickup/ghorg):
// Include 优先(非空时只留命中);Exclude 再剔除。
// IncludeGlobs/ExcludeGlobs 支持 * 通配;匹配对象是 key 或 name。
type Filter struct {
	Include      []string `yaml:"include" json:"include"`
	Exclude      []string `yaml:"exclude" json:"exclude"`
	IncludeGlobs []string `yaml:"include_globs" json:"include_globs"`
	ExcludeGlobs []string `yaml:"exclude_globs" json:"exclude_globs"`
}

// Allow 判断 key/name 是否通过过滤。
func (f *Filter) Allow(key, name string) bool {
	if f == nil {
		return true
	}
	if len(f.Include) == 0 && len(f.IncludeGlobs) == 0 {
		// 无 include 则默认全过,再走 exclude
		return !f.excluded(key, name)
	}
	included := f.included(key, name)
	if !included {
		return false
	}
	return !f.excluded(key, name)
}

func (f *Filter) included(key, name string) bool {
	for _, x := range f.Include {
		if x == key || x == name {
			return true
		}
	}
	for _, g := range f.IncludeGlobs {
		if pathx.MatchGlob(g, key) || pathx.MatchGlob(g, name) {
			return true
		}
	}
	return false
}

func (f *Filter) excluded(key, name string) bool {
	for _, x := range f.Exclude {
		if x == key || x == name {
			return true
		}
	}
	for _, g := range f.ExcludeGlobs {
		if pathx.MatchGlob(g, key) || pathx.MatchGlob(g, name) {
			return true
		}
	}
	return false
}
