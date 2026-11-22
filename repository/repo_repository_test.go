package repository

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/auth/credential"
	"gorm.io/gorm"
)

func TestCryptoManager_EncryptDecrypt_Roundtrip(t *testing.T) {
	// Set up a test encryption key
	key := "0123456789abcdef0123456789abcdef" // 32 bytes for AES-256
	t.Setenv("ENCRYPTION_KEY", key)

	cm, err := credential.NewCryptoManager()
	require.NoError(t, err, "failed to create CryptoManager")

	testCases := []struct {
		name      string
		plaintext string
	}{
		{"normal text", "my-secret-token-123"},
		{"with special chars", "token!@#$%^&*()_+-=[]{}|;':\",./<>?`~"},
		{"unicode", "token-with-unicode-\u4e2d\u6587"},
		{"long string", "a]very-long-token-that-is-quite-substantial-in-length-and-contains-many-characters-for-testing-purposes"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			encrypted, err := cm.Encrypt(tc.plaintext)
			require.NoError(t, err, "Encrypt failed")

			assert.NotEqual(t, tc.plaintext, encrypted, "encrypted text should differ from plaintext")

			decrypted, err := cm.Decrypt(encrypted)
			require.NoError(t, err, "Decrypt failed")

			assert.Equal(t, tc.plaintext, decrypted, "decrypt roundtrip failed")
		})
	}
}

func TestCryptoManager_EmptyString(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef"
	t.Setenv("ENCRYPTION_KEY", key)

	cm, err := credential.NewCryptoManager()
	require.NoError(t, err, "failed to create CryptoManager")

	// Test encrypting empty string
	encrypted, err := cm.Encrypt("")
	require.NoError(t, err, "Encrypt empty string failed")

	// Empty string should still produce some output (or handle gracefully)
	decrypted, err := cm.Decrypt(encrypted)
	require.NoError(t, err, "Decrypt empty string failed")

	assert.Equal(t, "", decrypted, "expected empty string")
}

func TestCryptoManager_WithoutKey(t *testing.T) {
	// Ensure ENCRYPTION_KEY is not set
	_ = os.Unsetenv("ENCRYPTION_KEY")

	_, err := credential.NewCryptoManager()
	assert.Error(t, err, "expected error when ENCRYPTION_KEY is not set")
}

func TestCryptoManager_NewCryptoManagerFromKey(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef"
	cm := credential.NewCryptoManagerFromKey(key)

	plaintext := "test-token-value"
	encrypted, err := cm.Encrypt(plaintext)
	require.NoError(t, err, "Encrypt failed")

	decrypted, err := cm.Decrypt(encrypted)
	require.NoError(t, err, "Decrypt failed")

	assert.Equal(t, plaintext, decrypted, "decrypt roundtrip failed")
}

func TestDefaultPagination(t *testing.T) {
	tests := []struct {
		name             string
		offset, limit    int
		wantOff, wantLim int
	}{
		{"normal values", 10, 50, 10, 50},
		{"zero limit defaults to 50", 0, 0, 0, 50},
		{"negative limit defaults to 50", 5, -1, 5, 50},
		{"limit over 200 capped", 0, 300, 0, 200},
		{"negative offset becomes 0", -5, 50, 0, 50},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultPagination(tc.offset, tc.limit)
			assert.Equal(t, tc.wantOff, p.Offset, "Offset mismatch")
			assert.Equal(t, tc.wantLim, p.Limit, "Limit mismatch")
		})
	}
}

func setupRepoTestDB(t *testing.T) (*gorm.DB, *RepoRepository) {
	t.Helper()
	// Set up encryption key for tests
	key := "0123456789abcdef0123456789abcdef" // 32 bytes for AES-256
	t.Setenv("ENCRYPTION_KEY", key)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err, "failed to open test db")

	err = db.AutoMigrate(&model.Repo{}, &model.Platform{})
	require.NoError(t, err, "failed to migrate test db")

	d, err := NewRepoRepository(db)
	require.NoError(t, err, "failed to create repo repository")

	return db, d
}

func TestRepoRepo_CreateAndFindByKey(t *testing.T) {
	_, d := setupRepoTestDB(t)

	// Create a repo
	repo := &model.Repo{
		Key:           "test-repo",
		Name:          "Test Repo",
		Platform:      "github",
		PlatformOwner: "testuser",
		PlatformRepo:  "test-repo",
		CloneURL:      "https://github.com/testuser/test-repo.git",
		DefaultBranch: "main",
		Status:        "active",
	}

	err := d.Create(repo)
	require.NoError(t, err, "create failed")

	assert.NotZero(t, repo.ID, "expected repo ID to be set after create")

	// Find by key
	got, err := d.FindByKey(context.Background(), "test-repo")
	require.NoError(t, err, "find by key failed")

	assert.Equal(t, "test-repo", got.Key, "expected key 'test-repo'")
	assert.Equal(t, "Test Repo", got.Name, "expected name 'Test Repo'")
	assert.Equal(t, "github", got.Platform, "expected platform 'github'")
	assert.Equal(t, "active", got.Status, "expected status 'active'")
}

func TestRepoRepo_FindAll(t *testing.T) {
	_, d := setupRepoTestDB(t)

	// Create multiple repos
	repos := []*model.Repo{
		{Key: "repo1", Name: "Repo 1", Platform: "github"},
		{Key: "repo2", Name: "Repo 2", Platform: "gitlab"},
		{Key: "repo3", Name: "Repo 3", Platform: "bitbucket"},
	}

	for _, repo := range repos {
		err := d.Create(repo)
		require.NoError(t, err, "create failed")
	}

	// Find all
	got, total, err := d.FindAll(context.Background(), DefaultPagination(0, 50))
	require.NoError(t, err, "find all failed")

	require.Equal(t, int64(3), total, "expected 3 repos")
	require.Len(t, got, 3, "expected 3 repos")
}

func TestRepoRepo_Update(t *testing.T) {
	_, d := setupRepoTestDB(t)

	// Create a repo
	repo := &model.Repo{
		Key:  "test-repo",
		Name: "Test Repo",
	}

	err := d.Create(repo)
	require.NoError(t, err, "create failed")

	// Update the repo
	repo.Name = "Updated Repo"
	repo.Status = "inactive"

	err = d.Update(repo)
	require.NoError(t, err, "update failed")

	// Get the updated repo
	got, err := d.FindByKey(context.Background(), "test-repo")
	require.NoError(t, err, "find by key failed")

	assert.Equal(t, "Updated Repo", got.Name, "expected name 'Updated Repo'")
	assert.Equal(t, "inactive", got.Status, "expected status 'inactive'")
}

func TestRepoRepo_BatchUpdateRepoMeta(t *testing.T) {
	_, d := setupRepoTestDB(t)

	repos := []*model.Repo{
		{Key: "repo-a", Name: "A", WebhookSecret: "secret-a"},
		{Key: "repo-b", Name: "B", WebhookSecret: "secret-b"},
		{Key: "repo-c", Name: "C", WebhookSecret: "secret-c"},
	}
	for _, r := range repos {
		require.NoError(t, d.Create(r))
	}

	// 只改元数据六列,加密/密钥列必须原样保留。
	repos[0].Name, repos[0].CloneURL, repos[0].DefaultBranch = "A2", "https://x/a.git", "trunk"
	repos[1].SSHURL, repos[1].PlatformOwner = "git@x:b.git", "owner-b"
	repos[2].PlatformRepo = "repo-c-remote"
	require.NoError(t, d.BatchUpdateRepoMeta(repos))

	want := map[string]struct{ name, clone, ssh, owner, repo, branch string }{
		"repo-a": {"A2", "https://x/a.git", "", "", "", "trunk"},
		"repo-b": {"B", "", "git@x:b.git", "owner-b", "", "main"},
		"repo-c": {"C", "", "", "", "repo-c-remote", "main"},
	}
	for key, w := range want {
		got, err := d.FindByKey(context.Background(), key)
		require.NoError(t, err)
		assert.Equal(t, w.name, got.Name, key)
		assert.Equal(t, w.clone, got.CloneURL, key)
		assert.Equal(t, w.ssh, got.SSHURL, key)
		assert.Equal(t, w.owner, got.PlatformOwner, key)
		assert.Equal(t, w.repo, got.PlatformRepo, key)
		assert.Equal(t, w.branch, got.DefaultBranch, key)
		assert.Equal(t, "secret-"+string(key[len(key)-1]), got.WebhookSecret, key)
	}
}

func TestRepoRepo_Delete(t *testing.T) {
	_, d := setupRepoTestDB(t)

	// Create a repo
	repo := &model.Repo{
		Key:  "test-repo",
		Name: "Test Repo",
	}

	err := d.Create(repo)
	require.NoError(t, err, "create failed")

	// Delete the repo
	err = d.Delete("test-repo")
	require.NoError(t, err, "delete failed")

	// Try to get the deleted repo
	got, err := d.FindByKey(context.Background(), "test-repo")
	require.ErrorIs(t, err, ErrNotFound)

	assert.Nil(t, got, "expected nil when getting deleted repo")
}

func TestRepoRepo_FindByKey_NotFound(t *testing.T) {
	_, d := setupRepoTestDB(t)

	// Try to get a non-existent repo
	got, err := d.FindByKey(context.Background(), "nonexistent")
	require.ErrorIs(t, err, ErrNotFound)

	assert.Nil(t, got, "expected nil when getting non-existent repo")
}

func TestRepoRepo_Count(t *testing.T) {
	_, d := setupRepoTestDB(t)

	// Create repos
	repos := []*model.Repo{
		{Key: "repo1", Name: "Repo 1"},
		{Key: "repo2", Name: "Repo 2"},
	}

	for _, repo := range repos {
		err := d.Create(repo)
		require.NoError(t, err, "create failed")
	}

	// Count
	count, err := d.Count()
	require.NoError(t, err, "count failed")

	assert.Equal(t, int64(2), count, "expected count 2")
}

func TestRepoRepo_Fields(t *testing.T) {
	repo := model.Repo{
		ID:            1,
		Key:           "test-repo",
		Name:          "Test Repo",
		Platform:      "github",
		PlatformOwner: "testuser",
		PlatformRepo:  "test-repo",
		CloneURL:      "https://github.com/testuser/test-repo.git",
		SSHURL:        "git@github.com:testuser/test-repo.git",
		DefaultBranch: "main",
		Status:        "active",
	}

	assert.Equal(t, uint(1), repo.ID, "expected ID 1")
	assert.Equal(t, "test-repo", repo.Key, "expected key 'test-repo'")
	assert.Equal(t, "Test Repo", repo.Name, "expected name 'Test Repo'")
	assert.Equal(t, "github", repo.Platform, "expected platform 'github'")
	assert.Equal(t, "testuser", repo.PlatformOwner, "expected platform owner 'testuser'")
	assert.Equal(t, "test-repo", repo.PlatformRepo, "expected platform repo 'test-repo'")
	assert.Equal(t, "https://github.com/testuser/test-repo.git", repo.CloneURL, "expected clone URL")
	assert.Equal(t, "git@github.com:testuser/test-repo.git", repo.SSHURL, "expected SSH URL")
	assert.Equal(t, "main", repo.DefaultBranch, "expected default branch 'main'")
	assert.Equal(t, "active", repo.Status, "expected status 'active'")
}

func TestRepoRepo_FindByPlatformID(t *testing.T) {
	db, d := setupRepoTestDB(t)

	// Create a platform
	platform := &model.Platform{
		Key:  "github",
		Name: "GitHub",
	}
	err := db.Create(platform).Error
	require.NoError(t, err, "create platform failed")

	// Create repos with different platform IDs
	repos := []*model.Repo{
		{Key: "repo1", Name: "Repo 1", PlatformID: platform.ID},
		{Key: "repo2", Name: "Repo 2", PlatformID: platform.ID},
		{Key: "repo3", Name: "Repo 3"}, // No platform
	}

	for _, repo := range repos {
		err := d.Create(repo)
		require.NoError(t, err, "create failed")
	}

	// Find by platform ID
	got, err := d.FindByPlatformID(platform.ID)
	require.NoError(t, err, "find by platform ID failed")

	require.Len(t, got, 2, "expected 2 repos")
}

func TestRepoRepo_ListWithFilter(t *testing.T) {
	_, d := setupRepoTestDB(t)

	// Create repos with different platforms
	repos := []*model.Repo{
		{Key: "repo1", Name: "Repo 1", Platform: "github", Status: "active"},
		{Key: "repo2", Name: "Repo 2", Platform: "github", Status: "active"},
		{Key: "repo3", Name: "Repo 3", Platform: "gitlab", Status: "inactive"},
	}

	for _, repo := range repos {
		err := d.Create(repo)
		require.NoError(t, err, "create failed")
	}

	// List with filter
	filter := &RepoFilter{
		Platform: "github",
	}

	got, total, err := d.ListWithFilter(context.Background(), DefaultPagination(0, 50), filter)
	require.NoError(t, err, "list with filter failed")

	require.Equal(t, int64(2), total, "expected 2 repos")
	require.Len(t, got, 2, "expected 2 repos")
}

func TestRepoRepo_ListWithFilter_Status(t *testing.T) {
	_, d := setupRepoTestDB(t)

	// Create repos with different statuses
	repos := []*model.Repo{
		{Key: "repo1", Name: "Repo 1", Platform: "github", Status: "active"},
		{Key: "repo2", Name: "Repo 2", Platform: "github", Status: "inactive"},
		{Key: "repo3", Name: "Repo 3", Platform: "gitlab", Status: "active"},
	}

	for _, repo := range repos {
		err := d.Create(repo)
		require.NoError(t, err, "create failed")
	}

	// List with status filter
	filter := &RepoFilter{
		Status: "active",
	}

	got, total, err := d.ListWithFilter(context.Background(), DefaultPagination(0, 50), filter)
	require.NoError(t, err, "list with filter failed")

	require.Equal(t, int64(2), total, "expected 2 repos")
	require.Len(t, got, 2, "expected 2 repos")
}

// TestRepoRepo_ListWithFilter_PagesNoDupNoLoss 钉住分页全序:
// 主排序列(status)值全相同时,跨页不得重复或丢行——依赖 id tiebreaker,
// 否则非唯一列排序下 offset 分页会因行漂移吞行/重行(gormx#269 同题)。
func TestRepoRepo_ListWithFilter_PagesNoDupNoLoss(t *testing.T) {
	_, d := setupRepoTestDB(t)

	for i := 1; i <= 5; i++ {
		err := d.Create(&model.Repo{Key: "repo" + strconv.Itoa(i), Name: "Repo " + strconv.Itoa(i), Platform: "github", Status: "active"})
		require.NoError(t, err, "create failed")
	}

	seen := map[uint]string{}
	for offset := 0; offset < 5; offset += 2 {
		got, total, err := d.ListWithFilter(context.Background(), DefaultPagination(offset, 2), &RepoFilter{SortBy: "status"})
		require.NoError(t, err, "list page offset=%d", offset)
		require.Equal(t, int64(5), total, "total must stay 5")
		for _, r := range got {
			prev, dup := seen[r.ID]
			require.False(t, dup, "row %d (key=%s) duplicated across pages (first seen as %s)", r.ID, r.Key, prev)
			seen[r.ID] = r.Key
		}
	}
	require.Len(t, seen, 5, "pages must cover all 5 rows exactly once")
}
