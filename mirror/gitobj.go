package mirror

import (
	"bytes"
	"fmt"
	"io"
	"regexp"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// rewriteModulePath 在 blob 数据中做模块路径边界改写。
// 替代 bytes.ReplaceAll——旧实现会把 github.com/a/b-extra 中的
// github.com/a/b 也替换,产生语义错误的 module path。
// 改用正则:源路径后必须跟非 Go 模块路径字符(或文本末尾)才替换,
// 使 github.com/a/b/pkg 仍被正确改写(子包路径),但 b-extra 不被误改。
var modulePathTerminator = regexp.MustCompile(`[^a-zA-Z0-9_.\-]|$`)

func rewriteModulePath(data []byte, source, target string) []byte {
	if source == "" || target == "" || !bytes.Contains(data, []byte(source)) {
		return data
	}
	// 匹配 "source + 边界字符",替换为 "target + 原边界字符"
	re := regexp.MustCompile(`(` + regexp.QuoteMeta(source) + `)(` + modulePathTerminator.String() + `)`)
	return re.ReplaceAll(data, []byte(target+"$2"))
}

// maxRewriteSize 是参与身份改写扫描的 blob 大小上限;超过者按原样保留
// (二进制大文件不可能承载模块身份,与"替换范围排除二进制"一致,
// 同时避免大仓库的内存放大)。
const maxRewriteSize = 8 << 20

// builder 从 canonical 仓库只读地复制源树结构,把发生身份改写的 blob
// 替换为新 blob,合成镜像 tree 写入临时仓库 storer。目录项的顺序、名字、
// mode 原样保留——源树本身满足 git 规范排序,因此合成 tree 天然规范。
type builder struct {
	repo     *git.Repository
	storer   storer.EncodedObjectStorer
	mapping  Mapping
	replaced []string
}

// rewriteTree 递归改写以 srcHash 为根的子树,prefix 是仓库内相对路径前缀
// (用于报告)。返回合成子树的 hash。
func (b *builder) rewriteTree(srcHash plumbing.Hash, prefix string) (plumbing.Hash, error) {
	srcTree, err := b.repo.TreeObject(srcHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("读取源树 %s 失败: %w", srcHash, err)
	}

	entries := make([]object.TreeEntry, len(srcTree.Entries))
	copy(entries, srcTree.Entries)
	for i, entry := range srcTree.Entries {
		path := prefix + entry.Name
		switch entry.Mode {
		case filemode.Dir:
			h, err := b.rewriteTree(entry.Hash, path+"/")
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries[i].Hash = h
		case filemode.Regular, filemode.Executable:
			data, err := b.blobBytes(entry.Hash)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			if len(data) > maxRewriteSize ||
				bytes.IndexByte(data, 0) >= 0 ||
				!bytes.Contains(data, []byte(b.mapping.Source)) {
				// 二进制/超限/无引用:原样保留,并把对象复制进临时仓库,
				// 保证物化与推送时对象完整
				if err := b.copyBlobObject(entry.Hash); err != nil {
					return plumbing.ZeroHash, err
				}
				continue
			}
			// 模块路径边界改写:旧实现 bytes.ReplaceAll 会把 github.com/a/b-extra
			// 中的 github.com/a/b 也替换为 github.com/x/y(产生错误路径)。
			// 改用正则:源路径后必须跟非 Go 模块路径字符(或文本末尾)才替换,
			// 使 github.com/a/b/pkg 仍被正确改写(子包),但 b-extra 不被误改。
			rewritten := rewriteModulePath(data, b.mapping.Source, b.mapping.Target)
			h, err := b.writeBlob(rewritten)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries[i].Hash = h
			b.replaced = append(b.replaced, path)
		default:
			// symlink 需要其目标 blob;gitlink(子模块)指向外部仓库,
			// git 推送本就不携带子模块对象
			if entry.Mode == filemode.Symlink {
				if err := b.copyBlobObject(entry.Hash); err != nil {
					return plumbing.ZeroHash, err
				}
			}
		}
	}

	tree := &object.Tree{Entries: entries}
	enc := b.storer.NewEncodedObject()
	if err := tree.Encode(enc); err != nil {
		return plumbing.ZeroHash, err
	}
	return b.storer.SetEncodedObject(enc)
}

func (b *builder) blobBytes(h plumbing.Hash) ([]byte, error) {
	blob, err := b.repo.BlobObject(h)
	if err != nil {
		return nil, fmt.Errorf("读取 blob %s 失败: %w", h, err)
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func (b *builder) writeBlob(data []byte) (plumbing.Hash, error) {
	enc := b.storer.NewEncodedObject()
	enc.SetType(plumbing.BlobObject)
	enc.SetSize(int64(len(data)))
	w, err := enc.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return b.storer.SetEncodedObject(enc)
}

// copyBlobObject 把 canonical 对象库中的 blob 原样复制进临时仓库。
func (b *builder) copyBlobObject(h plumbing.Hash) error {
	src, err := b.repo.Storer.EncodedObject(plumbing.BlobObject, h)
	if err != nil {
		return fmt.Errorf("读取 blob %s 失败: %w", h, err)
	}
	dst := b.storer.NewEncodedObject()
	dst.SetType(plumbing.BlobObject)
	dst.SetSize(src.Size())
	rd, err := src.Reader()
	if err != nil {
		return err
	}
	defer func() { _ = rd.Close() }()
	w, err := dst.Writer()
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, rd); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	_, err = b.storer.SetEncodedObject(dst)
	return err
}

// synthesizeRefs 在临时仓库中合成孤儿 commit(无父提交,快照语义)与
// 附注 tag。作者/提交者身份与时间戳从源 commit 原样继承,消息只由
// tag 与两个 module path 决定,因此同 tag 重跑 hash 完全一致。
func synthesizeRefs(st storer.EncodedObjectStorer, src *object.Commit, treeHash plumbing.Hash, tag string, m Mapping) (commit, tagHash plumbing.Hash, err error) {
	c := &object.Commit{
		Author:    src.Author,
		Committer: src.Committer,
		TreeHash:  treeHash,
		Message:   fmt.Sprintf("mirror %s: from %s, rewritten as %s\n", tag, m.Source, m.Target),
	}
	enc := st.NewEncodedObject()
	if err = c.Encode(enc); err != nil {
		return
	}
	if commit, err = st.SetEncodedObject(enc); err != nil {
		return
	}

	t := &object.Tag{
		Name:       tag,
		Tagger:     src.Committer,
		Message:    fmt.Sprintf("%s@%s (mirror of %s %s)\n", m.Target, tag, m.Source, tag),
		TargetType: plumbing.CommitObject,
		Target:     commit,
	}
	enc = st.NewEncodedObject()
	if err = t.Encode(enc); err != nil {
		return
	}
	tagHash, err = st.SetEncodedObject(enc)
	return
}
