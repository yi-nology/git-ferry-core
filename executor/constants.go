package executor

import "github.com/yi-nology/git-ferry-core/model"

// Git remote name constants
const (
	RemoteOrigin = "origin"
	RemoteTarget = "target"
)

// Work directory subdirectory name
const (
	// 与 service.GetRepoDir 同一单源(model.RepoDirName)。
	RepoDir = model.RepoDirName
)
