package mirror

import (
	"fmt"
	"strings"
)

// validateName 校验将被用作 git 引用/远端名的输入:只允许版本标签与
// 远端命名的安全字符集,禁止 "-" 开头、"..", "@{", 控制字符与空白,
// 以及 ".lock" 等非法结尾,使其不可能被解析为选项或 refspec 语法。
func validateName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s 不能为空", kind)
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("%s 不能以 '-' 开头: %q", kind, name)
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return fmt.Errorf("%s 含非法序列: %q", kind, name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '/':
		default:
			return fmt.Errorf("%s 含非法字符 %q: %q", kind, string(r), name)
		}
	}
	if strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("%s 以非法后缀结尾: %q", kind, name)
	}
	return nil
}

// validateVersion 校验语义化版本字符串(允许 +incompatible 等后缀)。
func validateVersion(version string) error {
	if version == "" {
		return fmt.Errorf("version 不能为空")
	}
	if strings.HasPrefix(version, "-") {
		return fmt.Errorf("version 不能以 '-' 开头: %q", version)
	}
	for _, r := range version {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '+':
		default:
			return fmt.Errorf("version 含非法字符 %q: %q", string(r), version)
		}
	}
	return nil
}
