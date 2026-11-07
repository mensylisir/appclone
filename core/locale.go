package core

import (
	"os"

	"github.com/mensylisir/appclone/core/i18n"
)

// isZhLocale 决定 Name[lang] 取哪一份。规则本身在 i18n 包里（ADR-025），
// core 只问"是不是简体中文"，不自己解析 locale 字符串——两处解析必然不一致。
func isZhLocale() bool { return i18n.DetectLang(os.Getenv) == "zh_CN" }
