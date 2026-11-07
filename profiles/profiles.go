// Package profiles 是内置档案模板库：随发行分发、社区可贡献，安装路径无关
// （//go:embed 编进二进制）。它与运行时档案 profiles.d/ 是两件事：
// 同名时以 profiles.d/ 为准，模板只作首次生成的预填来源（§4 三个名字的语义）。
package profiles

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed *.toml
var files embed.FS

// Get 返回某个应用的内置模板内容。没有该应用时返回 ok=false——
// 这不是错误：绝大多数应用都要靠探测管线（discover.md）现摸路径。
func Get(app string) (string, bool) {
	data, err := files.ReadFile(app + ".toml")
	if err != nil {
		return "", false
	}
	return string(data), true
}

// Names 返回内置模板覆盖的应用 key 列表（doctor 与文档用）。
func Names() []string {
	ents, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, ent := range ents {
		name := ent.Name()
		if strings.HasSuffix(name, ".toml") {
			out = append(out, strings.TrimSuffix(name, ".toml"))
		}
	}
	return out
}
