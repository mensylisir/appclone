// Package i18n 是 core 与两个视图共用的消息表。
//
// core 只产出 i18n 键（err.profile.public_path 这类），人类文案一律在这里拼：
// 同一条错误在 CLI 与 GUI 必须是同一句话，否则用户拿界面上的提示去搜文档、
// 搜到的是命令行的另一种说法。
package i18n

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Lang 是当前界面语言。v1 只出 zh_CN 与 en 两套。
func Lang() string {
	return DetectLang(os.Getenv)
}

// DetectLang 按 ADR-025 的规则选语言：LC_ALL > LC_MESSAGES > LANG > en；
// 剥离 .encoding 与 @modifier，`-` 归一为 `_`；zh_CN/zh_SG/zh_Hans* → 简体，
// zh_TW/zh_HK/zh_Hant* → 英文（v1 不做繁体，但不能因为 zh 前缀就给简体文案，
// 那是"看起来本地化了、实际混着两种语言"）。
func DetectLang(getenv func(string) string) string {
	raw := ""
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := getenv(k); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return "en"
	}
	if i := strings.IndexByte(raw, '.'); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.IndexByte(raw, '@'); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.ReplaceAll(raw, "-", "_")
	switch {
	case raw == "zh_CN", raw == "zh_SG", strings.HasPrefix(raw, "zh_Hans"):
		return "zh_CN"
	default:
		return "en"
	}
}

// T 渲染一条消息。缺键时返回键本身——宁可界面上出现 err.xxx.yyy 这种
// 明显的占位符，也不要静默显示一句不相关的中文（那会让缺陷藏起来）。
func T(lang, key string, args map[string]any) string {
	table, ok := messages[lang]
	if !ok {
		table = messages["en"]
	}
	tmpl, ok := table[key]
	if !ok {
		if fallback, ok2 := messages["en"][key]; ok2 {
			tmpl = fallback
		} else {
			return key
		}
	}
	if len(args) == 0 {
		return tmpl
	}
	return render(tmpl, args)
}

// render 支持 {name} 占位符；切片参数渲染成 "a、b、c"。
func render(tmpl string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := tmpl
	for _, k := range keys {
		out = strings.ReplaceAll(out, "{"+k+"}", formatArg(args[k]))
	}
	return out
}

func formatArg(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []string:
		return strings.Join(x, "、")
	case int:
		return fmt.Sprintf("%d", x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", x)
	}
}
