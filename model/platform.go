package model

import (
	"time"

	"github.com/yi-nology/git-platform-sdk/provider"
	"gorm.io/gorm"
)

// Platform 存储 Git 平台配置
type Platform struct {
	ID             uint           `json:"id" gorm:"primaryKey"`
	Key            string         `json:"key" gorm:"uniqueIndex;size:255;not null"`
	Name           string         `json:"name" gorm:"size:100;not null"`
	Type           string         `json:"type" gorm:"size:50;not null"`           // github, gitlab, gitea, gitee, gitcode, atomgit, tencent_code, custom
	InstanceURL    string         `json:"instance_url" gorm:"size:255"`           // 实例地址，如 github.com, gitlab.company.com
	APIURL         string         `json:"api_url" gorm:"size:255;not null"`       // API 地址，如 https://api.github.com
	AccessToken    string         `json:"-" gorm:"type:text"`                     // 访问令牌（加密存储）
	SkipTLSVerify  bool           `json:"skip_tls_verify" gorm:"default:false"`   // 跳过 TLS 证书验证
	CACertPath     string         `json:"ca_cert_path" gorm:"size:500"`           // 自定义 CA 证书路径
	ProxyURL       string         `json:"proxy_url" gorm:"size:255"`              // HTTP 代理地址
	// SSHHostKeyFingerprint SSH 主机公钥指纹钉扎("SHA256:xxx");设置后优先于 known_hosts
	SSHHostKeyFingerprint string `json:"ssh_host_key_fingerprint" gorm:"size:128"`
	// SSHKnownHostsPath 平台级 known_hosts 路径(空=用 ~/.ssh/known_hosts)
	SSHKnownHostsPath string `json:"ssh_known_hosts_path" gorm:"size:500"`
	IsDefault      bool           `json:"is_default" gorm:"default:false"`        // 是否为默认平台
	Status         string         `json:"status" gorm:"size:20;default:active"`   // 状态: active, error
	LastTestAt     *time.Time     `json:"last_test_at"`                           // 最后测试时间
	LastTestResult string         `json:"last_test_result" gorm:"size:500"`       // 最后测试结果
	RepoCount      int            `json:"repo_count" gorm:"default:0"`            // 关联的仓库数量
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`
}

func (Platform) TableName() string {
	return "platforms"
}

// 平台类型常量:SDK 已注册平台直接复用 provider.Platform 常量(字符串值相同);
// SDK 未注册的扩展类型保留本地定义。
const (
	PlatformTypeGitHub      = string(provider.PlatformGitHub)      // "github"
	PlatformTypeGitLab      = string(provider.PlatformGitLab)      // "gitlab"
	PlatformTypeGitea       = string(provider.PlatformGitea)       // "gitea"
	PlatformTypeGitee       = string(provider.PlatformGitee)       // "gitee"
	PlatformTypeGitCode     = string(provider.PlatformGitCode)     // "gitcode"
	PlatformTypeTencentCode = string(provider.PlatformTencentCode) // "tencent_code"
	// SDK 未注册的扩展类型
	PlatformTypeAtomGit = "atomgit"
	PlatformTypeCustom  = "custom"
)

// PlatformStatus 平台状态常量
const (
	PlatformStatusActive = "active"
	PlatformStatusError  = "error"
)

// ValidPlatformType 检查平台类型是否合法:SDK 已注册平台 + 扩展白名单。
// 替代旧的 ValidPlatformTypes 静态 map,与 SDK 注册表自动同步。
func ValidPlatformType(t string) bool {
	return provider.IsRegistered(provider.Platform(t)) || extensionPlatforms[t]
}

// extensionPlatforms SDK 未注册但业务支持的扩展平台。
var extensionPlatforms = map[string]bool{
	PlatformTypeAtomGit: true,
	PlatformTypeCustom:  true,
}

// PlatformAPIPaths 各平台的 API 路径(壳层 GetAPIURL 用)。
// SDK 已注册平台的值作为已知默认;扩展平台是唯一数据源。
var PlatformAPIPaths = map[string]string{
	PlatformTypeGitHub:      "/api/v3",
	PlatformTypeGitLab:      "/api/v4",
	PlatformTypeGitea:       "/api/v1",
	PlatformTypeGitee:       "/api/v5",
	PlatformTypeGitCode:     "/api/v5",
	PlatformTypeAtomGit:     "/api/v1",
	PlatformTypeTencentCode: "/api/v3",
}

// PlatformDefaultInstances 各平台的默认实例地址。
var PlatformDefaultInstances = map[string]string{
	PlatformTypeGitHub:      "github.com",
	PlatformTypeGitLab:      "gitlab.com",
	PlatformTypeGitea:       "gitea.com",
	PlatformTypeGitee:       "gitee.com",
	PlatformTypeGitCode:     "gitcode.com",
	PlatformTypeAtomGit:     "atomgit.com",
	PlatformTypeTencentCode: "git.code.tencent.com",
}

// GetAPIURL 根据实例地址生成 API URL。
// 自建实例(非默认域名)的 URL 由壳层传入 instanceURL 覆盖;
// SDK 的 provider.NewProvider 也能推导已知平台的 URL,但 DB 字段 NOT NULL
// 要求壳层在创建时就提供非空值,因此保留此辅助函数。
func GetAPIURL(platformType, instanceURL string) string {
	if instanceURL == "" {
		instanceURL = PlatformDefaultInstances[platformType]
	}
	apiPath := PlatformAPIPaths[platformType]
	if apiPath == "" {
		return ""
	}
	return "https://" + instanceURL + apiPath
}