package service

import "testing"

func TestResolveOrgTarget(t *testing.T) {
	cases := []struct {
		strategy OrgMapStrategy
		srcOwner, srcRepo, targetOrg, targetUser string
		personal bool
		wantOwner, wantRepo string
	}{
		{OrgMapPreserve, "acme", "api", "", "", false, "acme", "api"},
		{OrgMapPreserve, "alice", "dot", "", "alice", true, "alice", "dot"},
		{OrgMapSingle, "acme", "api", "backup", "", false, "backup", "api"},
		{OrgMapSingle, "alice", "dot", "backup", "", true, "backup", "dot"},
		{OrgMapFlat, "acme", "api", "", "mirror", false, "mirror", "api"},
		{OrgMapMixed, "alice", "dot", "backup", "mirror", true, "mirror", "dot"},
		{OrgMapMixed, "acme", "api", "backup", "mirror", false, "backup", "api"},
	}
	for i, c := range cases {
		o, r := ResolveOrgTarget(c.strategy, c.srcOwner, c.srcRepo, c.targetOrg, c.targetUser, c.personal)
		if o != c.wantOwner || r != c.wantRepo {
			t.Fatalf("case %d: got %s/%s want %s/%s", i, o, r, c.wantOwner, c.wantRepo)
		}
	}
}

func TestValidOrgMapStrategy(t *testing.T) {
	for _, s := range []string{"", "preserve", "single", "flat", "mixed"} {
		if !ValidOrgMapStrategy(s) {
			t.Fatalf("want valid %q", s)
		}
	}
	if ValidOrgMapStrategy("nope") {
		t.Fatal("want invalid")
	}
}
