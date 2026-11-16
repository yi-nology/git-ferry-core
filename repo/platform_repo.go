package repo

import (
	"github.com/yi-nology/go-git-platform/auth/credential"

	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// PlatformRepo 平台数据访问对象。
// AccessToken 与 repo_dao 一致采用加密存储:写入加密、读取解密;
// 读取解密失败视为存量明文,原样返回(下次写入时自动转为密文)。
type PlatformRepo struct {
	base[model.Platform, uint]
	cm *credential.CryptoManager
}

// NewPlatformRepo 创建 PlatformRepo
func NewPlatformRepo(db *gorm.DB) (*PlatformRepo, error) {
	cm, err := credential.NewCryptoManager()
	if err != nil {
		return nil, oekiterrors.Wrap(err, "failed to create CryptoManager")
	}
	return &PlatformRepo{base: newBase[model.Platform, uint](db), cm: cm}, nil
}

// decrypt 就地解密平台 AccessToken 与 GitHub App 私钥(解密失败保留原文,兼容存量明文)
func (d *PlatformRepo) decrypt(p *model.Platform) {
	if p == nil {
		return
	}
	if p.AccessToken != "" {
		if decrypted, err := d.cm.Decrypt(p.AccessToken); err == nil {
			p.AccessToken = decrypted
		}
	}
	if p.GitHubPrivateKey != "" {
		if decrypted, err := d.cm.Decrypt(p.GitHubPrivateKey); err == nil {
			p.GitHubPrivateKey = decrypted
		}
	}
}

// decryptAll 就地解密平台列表
func (d *PlatformRepo) decryptAll(platforms []*model.Platform) {
	for _, p := range platforms {
		d.decrypt(p)
	}
}

// encrypt 就地加密平台 AccessToken 与 GitHub App 私钥(空值跳过)
func (d *PlatformRepo) encrypt(p *model.Platform) error {
	if p == nil {
		return nil
	}
	if p.AccessToken != "" {
		encrypted, err := d.cm.Encrypt(p.AccessToken)
		if err != nil {
			return oekiterrors.Wrap(err, "failed to encrypt access token")
		}
		p.AccessToken = encrypted
	}
	if p.GitHubPrivateKey != "" {
		encrypted, err := d.cm.Encrypt(p.GitHubPrivateKey)
		if err != nil {
			return oekiterrors.Wrap(err, "failed to encrypt github app private key")
		}
		p.GitHubPrivateKey = encrypted
	}
	return nil
}

// Create 创建平台
func (d *PlatformRepo) Create(platform *model.Platform) error {
	if err := d.encrypt(platform); err != nil {
		return err
	}
	return d.create(platform)
}

// FindByKey 根据 Key 查找平台(not found 返回 nil,nil,与 RepoRepo 行为一致)。
func (d *PlatformRepo) FindByKey(key string) (*model.Platform, error) {
	platform, err := d.getByCondition("key = ?", key)
	if err != nil || platform == nil {
		return platform, err
	}
	d.decrypt(platform)
	return platform, nil
}

// FindByID 根据 ID 查找平台(not found 返回 nil,nil,与 RepoRepo 行为一致)。
func (d *PlatformRepo) FindByID(id uint) (*model.Platform, error) {
	platform, err := d.getByID(id)
	if err != nil || platform == nil {
		return platform, err
	}
	d.decrypt(platform)
	return platform, nil
}

// FindAll 查找所有平台
func (d *PlatformRepo) FindAll() ([]*model.Platform, error) {
	var platforms []*model.Platform
	err := d.db.Order("is_default DESC, name ASC").Find(&platforms).Error
	if err != nil {
		return nil, err
	}
	d.decryptAll(platforms)
	return platforms, nil
}

// FindByType 根据类型查找平台
func (d *PlatformRepo) FindByType(platformType string) ([]*model.Platform, error) {
	var platforms []*model.Platform
	err := d.db.Where("type = ?", platformType).Find(&platforms).Error
	if err != nil {
		return nil, err
	}
	d.decryptAll(platforms)
	return platforms, nil
}

// Update 更新平台(列白名单:不含主键/审计列,也不会 upsert)
func (d *PlatformRepo) Update(platform *model.Platform) error {
	if err := d.encrypt(platform); err != nil {
		return err
	}
	return d.update(platform)
}

// UpdateFields 更新平台指定字段
// 注意:字段值原样写入;如需更新 access_token 请走 Update 以确保加密。
func (d *PlatformRepo) UpdateFields(key string, fields map[string]interface{}) error {
	return d.db.Model(&model.Platform{}).Where("key = ?", key).Updates(fields).Error
}

// Delete 删除平台（软删除）
func (d *PlatformRepo) Delete(key string) error {
	return d.db.Where("key = ?", key).Delete(&model.Platform{}).Error
}

// SetDefault 设置默认平台(事务保证原子性:先清后设,不会出现无默认的窗口)
func (d *PlatformRepo) SetDefault(key string) error {
	return d.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Platform{}).Where("is_default = ?", true).Update("is_default", false).Error; err != nil {
			return err
		}
		return tx.Model(&model.Platform{}).Where("key = ?", key).Update("is_default", true).Error
	})
}

// UpdateRepoCount 更新平台关联的仓库数量
func (d *PlatformRepo) UpdateRepoCount(platformID uint) error {
	var count int64
	err := d.db.Model(&model.Repo{}).Where("platform_id = ? AND deleted_at IS NULL", platformID).Count(&count).Error
	if err != nil {
		return err
	}
	return d.db.Model(&model.Platform{}).Where("id = ?", platformID).Update("repo_count", count).Error
}

// UpdateStatus 更新平台状态
func (d *PlatformRepo) UpdateStatus(key, status, testResult string) error {
	return d.db.Model(&model.Platform{}).Where("key = ?", key).Updates(map[string]interface{}{
		"status":           status,
		"last_test_at":     gorm.Expr("CURRENT_TIMESTAMP"),
		"last_test_result": testResult,
	}).Error
}

// Count 统计平台数量
func (d *PlatformRepo) Count() (int64, error) {
	var count int64
	err := d.db.Model(&model.Platform{}).Count(&count).Error
	return count, err
}
