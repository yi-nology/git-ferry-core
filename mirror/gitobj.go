package mirror

import (
	"bytes"
	"fmt"
	"io"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// rewriteModulePath 在 blob 数据中做模块路径边界改写。
// 替代 bytes.ReplaceAll——旧实现会把 github.com/a/b-extra 中的
// github.com/a/b 也替换,产生语义错误的 module path。
//
// 边界语义现由 rewrite.go 的 Rewriter 统一实现(KindModule):
// 源路径后必须跟非 Go 模块路径字符(或文本末尾)才替换,
// 使 github.com/a/b/pkg 仍被正确改写(子包路径),但 b-extra 不被误改。
// 单一实现:改写规则只存在于 Rewriter 一处,publish 模式与历史同步共用。
func rewriteModulePath(data []byte, source, target string) []byte {
	out, err := RewriteIdentities(data, []IdentityRule{{
		Source: source,
		Target: target,
		Kind:   KindModule,
	}})
	if err != nil {
		return data
	}
	return out
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
	replayer *Rewriter
	// replaced 是**内容真的被改写**的文件(仓库内相对路径)。
	replaced []string
	// residual 是**仍带着内网标识**的文件:规则本该改写,却因
	// 二进制/超限而原样保留。发布阶段会因此中止。
	residual []string
	// scrubRules 是痕迹清理规则(见 Options.ScrubRules)。
	scrubRules []ScrubRule
	// scrubHits 是痕迹清理的命中(逐条带规则名与行号)。
	scrubHits []ScrubHit
	// mandatoryHits 是**强制人工处理**的命中(密钥类)——
	// 发布阶段会因此中止(密钥不进公开 module)。
	mandatoryHits []string
}

// newBuilder 建一个改写用的 builder。
//
// 编译失败直接报错:规则非法时宁可不发,也不发一棵没改干净的树。
func newBuilder(repo *git.Repository, st storer.EncodedObjectStorer, m Mapping, rules []ScrubRule) (*builder, error) {
	rw, err := NewRewriter([]IdentityRule{{
		Source: m.Source,
		Target: m.Target,
		Kind:   KindModule,
	}})
	if err != nil {
		return nil, err
	}
	return &builder{repo: repo, storer: st, mapping: m, replayer: rw, scrubRules: rules}, nil
}

// scrub 对单个文件做痕迹清理,记录命中。
//
// **顺序在改写之后** —— 规则对改写后的内容生效(与历史同步侧一致)。
// 强制人工处理的命中(密钥类)不改内容,但**必须中止发布**:
// 留痕不等于可以发布 —— 密钥进公开 module 就是泄漏。
func (b *builder) scrub(path string, data []byte) []byte {
	if len(b.scrubRules) == 0 {
		return data
	}
	res := ApplyScrub(data, b.scrubRules)
	for _, h := range res.Hits {
		b.scrubHits = append(b.scrubHits, h)
		if h.Mandatory {
			b.mandatoryHits = append(b.mandatoryHits,
				fmt.Sprintf("%s:%d(%s)", path, h.Line, h.Rule))
		}
	}
	return res.Content
}

// checkMandatoryOnly 对**不能改写**的 blob(二进制/超限)跑一遍 mandatory 规则。
//
// 只查密钥类(mandatory):auto 规则要改写内容,而二进制改写等于破坏文件。
// 但密钥类恰恰是"绝不能进公开仓"的那类 —— 不能因为文件是二进制就放行。
func (b *builder) checkMandatoryOnly(path string, data []byte) {
	if len(b.scrubRules) == 0 {
		return
	}
	mandatory := make([]ScrubRule, 0, len(b.scrubRules))
	for _, r := range b.scrubRules {
		if r.Mandatory() {
			mandatory = append(mandatory, r)
		}
	}
	if len(mandatory) == 0 {
		return
	}
	res := ApplyScrub(data, mandatory)
	for _, h := range res.Hits {
		b.scrubHits = append(b.scrubHits, h)
		b.mandatoryHits = append(b.mandatoryHits,
			fmt.Sprintf("%s:%d(%s)", path, h.Line, h.Rule))
	}
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
			// **「原样保留」不等于「里面没有内网标识」**。
			//
			// 二进制/超限的 blob 走原样保留路径,但它照样可能写着内网
			// module path —— 原样保留 = 把内网标识发到公开仓。
			// 所以残留判据不能是「文件大不大」,而是
			// 「**规则本该改写这里,却没改写**」(replayer.Pending)。
			oversized := len(data) > maxRewriteSize
			binary := bytes.IndexByte(data, 0) >= 0
			if oversized || binary {
				if b.replayer.Pending(data) {
					// 记残留:发布阶段会因此中止并把文件列出来。
					b.residual = append(b.residual, path)
				}
				// **不能改写 ≠ 可以不管**:二进制/超限的 blob 仍要跑一遍
				// mandatory 规则。此前这条路径完全跳过 scrub ⇒ 二进制里的
				// 密钥(ghp_xxx 等)原样进公开 module,而密钥类规则一次都不会
				// 触发 —— 规则形同虚设。
				// auto 类规则对二进制无意义(改写二进制会破坏它),故只查 mandatory。
				b.checkMandatoryOnly(path, data)
				// 把对象复制进临时仓库,保证物化与推送时对象完整。
				if err := b.copyBlobObject(entry.Hash); err != nil {
					return plumbing.ZeroHash, err
				}
				continue
			}
			// 模块路径边界改写:旧实现 bytes.ReplaceAll 会把 github.com/a/b-extra
			// 中的 github.com/a/b 也替换为 github.com/x/y(产生错误路径)。
			// 改用统一改写器:源路径后必须跟非 Go 模块路径字符(或文本末尾)才替换,
			// 使 github.com/a/b/pkg 仍被正确改写(子包),但 b-extra 不被误改。
			// **发布形态 = 改写 + gofmt 归一化**(与历史同步同源):
			// 两条链路曾对同一份源码发布出不同的字节(import 顺序),
			// 公开仓的 gofmt CI 会挂,而类型门禁看不出来。
			rewritten := b.replayer.PublishForm(path, data)
			// **痕迹清理在改写之后**(规则对改写后的内容生效),
			// 顺序与历史同步侧一致:rewrite → gofmt → scrub。
			rewritten = b.scrub(path, rewritten)
			h, err := b.writeBlob(rewritten)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries[i].Hash = h
			// **只把「内容真的变了」的文件记成改写过**。
			// 旧实现无条件记账:一个因为边界不成立而原样保留的文件
			// 也会被列进「已改写」—— 报告说改了,实际没改。
			if !bytes.Equal(rewritten, data) {
				b.replaced = append(b.replaced, path)
			}
		default:
			// symlink 需要其目标 blob;gitlink(子模块)指向外部仓库,
			// git 推送本就不携带子模块对象
			if entry.Mode == filemode.Symlink {
				// **symlink 目标也要过改写**:目标是一个路径串,
				// 里面同样可能写着内网 module path(指向内网目录的软链)。
				// 不改写 = 内网标识照样进公开仓。
				data, err := b.blobBytes(entry.Hash)
				if err != nil {
					return plumbing.ZeroHash, err
				}
				rewritten := b.replayer.PublishForm(path, data)
				if bytes.Equal(rewritten, data) {
					if err := b.copyBlobObject(entry.Hash); err != nil {
						return plumbing.ZeroHash, err
					}
					continue
				}
				h, err := b.writeBlob(rewritten)
				if err != nil {
					return plumbing.ZeroHash, err
				}
				entries[i].Hash = h
				b.replaced = append(b.replaced, path)
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
