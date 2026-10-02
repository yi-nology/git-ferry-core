package service

import (
	"testing"

	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

func TestRepoImportFilter_Allow(t *testing.T) {
	repo := func(fn string, opt ...func(*sdkprov.PlatformRepo)) *sdkprov.PlatformRepo {
		r := &sdkprov.PlatformRepo{FullName: fn, Name: "name-fallback"}
		for _, o := range opt {
			o(r)
		}
		return r
	}
	archived := func(r *sdkprov.PlatformRepo) { r.Archived = true }
	forked := func(r *sdkprov.PlatformRepo) { r.Fork = true }
	stars := func(n int) func(*sdkprov.PlatformRepo) {
		return func(r *sdkprov.PlatformRepo) { r.Stars = n }
	}
	lang := func(l string) func(*sdkprov.PlatformRepo) {
		return func(r *sdkprov.PlatformRepo) { r.Language = l }
	}

	// nil filter 恒放行
	var nilFilter *RepoImportFilter
	if !nilFilter.Allow(repo("a/b")) {
		t.Fatal("nil filter 应放行")
	}

	f := &RepoImportFilter{ExcludeArchived: true, ExcludeForks: true, MinStars: 5, IncludeLanguage: "Go"}
	if f.Allow(repo("a/b", archived)) {
		t.Fatal("应排除 archived")
	}
	if f.Allow(repo("a/b", forked)) {
		t.Fatal("应排除 fork")
	}
	if f.Allow(repo("a/b", stars(3))) {
		t.Fatal("应排除 star 不足")
	}
	if f.Allow(repo("a/b", lang("GoLang"))) {
		t.Fatal("语言不匹配应排除")
	}
	if !f.Allow(repo("a/b", stars(10), lang("go"))) {
		t.Fatal("满足条件应放行")
	}

	// FullName 为空时回退 Name 匹配 glob
	f2 := &RepoImportFilter{IncludeGlobs: []string{"team-*"}}
	if !f2.Allow(repo("", func(r *sdkprov.PlatformRepo) { r.Name = "team-a" })) {
		t.Fatal("FullName 空时应回退 Name 匹配")
	}
	if f2.Allow(repo("other/x")) {
		t.Fatal("不在 include 列表应排除")
	}

	f3 := &RepoImportFilter{ExcludeGlobs: []string{"*/archived-*"}}
	if f3.Allow(repo("org/archived-old")) {
		t.Fatal("命中 exclude glob 应排除")
	}
	// include + exclude 同时给出：exclude 优先
	f4 := &RepoImportFilter{IncludeGlobs: []string{"org/*"}, ExcludeGlobs: []string{"org/skip-*"}}
	if f4.Allow(repo("org/skip-me")) {
		t.Fatal("exclude 应优先于 include")
	}
	if !f4.Allow(repo("org/keep")) {
		t.Fatal("include 命中且未被 exclude 应放行")
	}
}

func TestParsePageOpts(t *testing.T) {
	p, pp := parsePageOpts("", "")
	if p <= 0 || pp <= 0 {
		t.Fatalf("空参数应回落默认: %d,%d", p, pp)
	}
	p, pp = parsePageOpts("3", "50")
	if p != 3 || pp != 50 {
		t.Fatalf("got %d,%d", p, pp)
	}
	p, _ = parsePageOpts("-1", "abc")
	if p <= 0 {
		t.Fatalf("非法参数应回落默认: %d", p)
	}
}
