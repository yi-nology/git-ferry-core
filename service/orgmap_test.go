package service

import "testing"

func TestParseStrategy(t *testing.T) {
	for _, in := range []string{"", "preserve", "single", "flat", "mixed", " PRESERVE "} {
		s, err := ParseStrategy(in)
		if err != nil {
			t.Fatalf("ParseStrategy(%q): %v", in, err)
		}
		if s == "" {
			t.Fatalf("ParseStrategy(%q) empty", in)
		}
	}
	if _, err := ParseStrategy("nope"); err == nil {
		t.Fatal("ParseStrategy(nope) want error")
	}
}

func TestValidOrgMapStrategy(t *testing.T) {
	for _, s := range []string{"", "preserve", "single", "flat", "mixed"} {
		if !ValidOrgMapStrategy(s) {
			t.Fatalf("ValidOrgMapStrategy(%q) = false", s)
		}
	}
	if ValidOrgMapStrategy("nope") {
		t.Fatal("ValidOrgMapStrategy(nope) = true")
	}
}

// TestResolveOrgTarget_Import 语义：org_import 用法 —— preserve 恒等、
// single/flat/mixed 个人仓缺落点报错、mixed 组织仓保持源 owner。
func TestResolveOrgTarget_Import(t *testing.T) {
	base := OrgMapOptions{RequireTarget: true}
	cases := []struct {
		name                   string
		strategy               OrgMapStrategy
		owner, repo, org, user string
		isPersonal             bool
		wantOwner, wantRepo    string
		wantErr                bool
	}{
		{"preserve 恒等", OrgMapPreserve, "acme", "api", "backup", "", false, "acme", "api", false},
		{"single 落 target_org", OrgMapSingle, "acme", "api", "backup", "", false, "backup", "api", false},
		{"single 缺 target_org 报错", OrgMapSingle, "acme", "api", "", "", false, "", "", true},
		{"flat 落 target_user", OrgMapFlat, "acme", "api", "backup", "mirror", false, "mirror", "api", false},
		{"flat 缺 target_user 报错", OrgMapFlat, "acme", "api", "backup", "", false, "", "", true},
		{"mixed 个人仓落 target_user", OrgMapMixed, "alice", "dot", "backup", "alice", true, "alice", "dot", false},
		{"mixed 个人仓缺 target_user 报错", OrgMapMixed, "alice", "dot", "backup", "", true, "", "", true},
		{"mixed 组织仓保持源 owner", OrgMapMixed, "acme", "api", "backup", "alice", false, "acme", "api", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := base
			o.Strategy, o.TargetOrg, o.TargetUser, o.SourceIsPersonal = c.strategy, c.org, c.user, c.isPersonal
			got, err := ResolveOrgTarget(c.owner, c.repo, o)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Owner != c.wantOwner || got.Repo != c.wantRepo {
				t.Fatalf("got %s/%s want %s/%s", got.Owner, got.Repo, c.wantOwner, c.wantRepo)
			}
		})
	}
}

// TestResolveOrgTarget_Mirror 语义：org_mirror 用法 —— 缺落点静默回落、
// mixed 组织仓落 target_org、mixed 个人仓判定用 IsPersonalOwner。
func TestResolveOrgTarget_Mirror(t *testing.T) {
	cases := []struct {
		name                   string
		strategy               string
		owner, repo, org, user string
		wantOwner, wantRepo    string
	}{
		{"preserve 恒等", "preserve", "acme", "api", "", "", "acme", "api"},
		{"single 落 target_org", "single", "acme", "api", "backup", "", "backup", "api"},
		{"single 缺落点回落", "single", "acme", "api", "", "", "acme", "api"},
		{"flat 落 target_user", "flat", "acme", "api", "", "mirror", "mirror", "api"},
		{"mixed 个人仓", "mixed", "alice", "dot", "backup", "alice", "alice", "dot"},
		{"mixed 组织仓", "mixed", "acme", "api", "backup", "alice", "backup", "api"},
		{"mixed 组织仓缺 target_org 回落", "mixed", "acme", "api", "", "alice", "acme", "api"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			strategy, err := ParseStrategy(c.strategy)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ResolveOrgTarget(c.owner, c.repo, OrgMapOptions{
				Strategy:         strategy,
				TargetOrg:        c.org,
				TargetUser:       c.user,
				SourceIsPersonal: IsPersonalOwner(c.owner, c.user, nil),
				MixedOrgToTarget: true,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Owner != c.wantOwner || got.Repo != c.wantRepo {
				t.Fatalf("got %s/%s want %s/%s", got.Owner, got.Repo, c.wantOwner, c.wantRepo)
			}
		})
	}
}

// TestResolveOrgTarget_PreserveRedirect 覆盖旧版 core 的重定向语义。
func TestResolveOrgTarget_PreserveRedirect(t *testing.T) {
	got, err := ResolveOrgTarget("acme", "api", OrgMapOptions{
		Strategy: OrgMapPreserve, TargetOrg: "backup", PreserveRedirect: true,
	})
	if err != nil || got.Owner != "backup" {
		t.Fatalf("preserve redirect org: %+v err=%v", got, err)
	}
	got, err = ResolveOrgTarget("alice", "dot", OrgMapOptions{
		Strategy: OrgMapPreserve, TargetUser: "mirror", SourceIsPersonal: true, PreserveRedirect: true,
	})
	if err != nil || got.Owner != "mirror" {
		t.Fatalf("preserve redirect user: %+v err=%v", got, err)
	}
	got, err = ResolveOrgTarget("acme", "api", OrgMapOptions{
		Strategy: OrgMapPreserve, TargetOrg: "backup",
	})
	if err != nil || got.Owner != "acme" {
		t.Fatalf("preserve no redirect: %+v err=%v", got, err)
	}
}

// TestResolveOrgTarget_SourceKey 覆盖 owner/repo 为空时从 SourceKey 解析。
func TestResolveOrgTarget_SourceKey(t *testing.T) {
	got, err := ResolveOrgTarget("", "", OrgMapOptions{SourceKey: "github/acme/api", TargetPlatform: "gitlab"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "acme" || got.Repo != "api" || got.Key != "gitlab/acme/api" {
		t.Fatalf("got %+v", got)
	}
	got, err = ResolveOrgTarget("", "", OrgMapOptions{SourceKey: "acme/api"})
	if err != nil || got.Owner != "acme" || got.Repo != "api" || got.Key != "" {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if _, err = ResolveOrgTarget("", "", OrgMapOptions{}); err == nil {
		t.Fatal("empty SourceKey want error")
	}
	if _, err = ResolveOrgTarget("", "", OrgMapOptions{SourceKey: "only"}); err == nil {
		t.Fatal("bad SourceKey want error")
	}
}

func TestIsPersonalOwner(t *testing.T) {
	orgs := map[string]bool{"acme": true}
	if IsPersonalOwner("acme", "alice", orgs) {
		t.Fatal("known org judged personal")
	}
	if !IsPersonalOwner("alice", "alice", orgs) {
		t.Fatal("owner==targetUser should be personal")
	}
	if IsPersonalOwner("bob", "alice", orgs) {
		t.Fatal("bob != alice not personal")
	}
	if IsPersonalOwner("alice", "", orgs) {
		t.Fatal("empty targetUser cannot judge personal")
	}
}
