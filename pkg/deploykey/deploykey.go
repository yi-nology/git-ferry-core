// Package deploykey 生成镜像部署用 Ed25519 密钥对（壳层 ops/deploy-key 端点下沉）。
//
// 私钥只在响应中返回一次、不落库；调用方自行存入密钥管理。
package deploykey

import "github.com/yi-nology/go-git-platform/pkg/credential"

// Generate 生成 comment 标识的 Ed25519 部署密钥对。
func Generate(comment string) (*credential.DeployKey, error) {
	return credential.GenerateEd25519DeployKey(comment)
}
