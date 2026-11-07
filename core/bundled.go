package core

import "github.com/mensylisir/appclone/profiles"

// BundledProfile 取内置模板并解析成 Profile（§4 三个名字里的"仓库内模板库"）。
//
// 它只是"从没探测过时"的起点：probe_method 固定记 manual，界面据此把置信度
// 标成"来自内置模板，未在本机验证过"——不能让用户以为模板等于探测结果。
func (e *Env) BundledProfile(app string) (Profile, bool, error) {
	raw, ok := profiles.Get(app)
	if !ok {
		return Profile{}, false, nil
	}
	p, err := parseProfile(app, "profiles/"+app+".toml", []byte(raw))
	if err != nil {
		return Profile{}, false, err
	}
	p.App = app
	p.ProbeMethod = "manual"
	return p, true, nil
}
