#!/bin/sh
# appclone 安装后动作（deb / rpm / Arch 共用）：
#   ① Ubuntu 23.10+ 的 AppArmor userns 限制：只给 bwrap 装一条 unconfined profile，
#      不改全局 sysctl；目标文件已存在时默认跳过（让位给发行版自己的版本）。
#      先写临时文件再 rename，安装中断不会留下半截 profile。
#      升级迁移：v1.0.0 装的同名文件没有 appclone-owned 标记，postremove 认不出、
#      永远不会回收。这里只在"已存在文件与随包模板除标记行外逐字节一致"时
#      补写标记——发行版/其他包的同名文件内容不同，一律不碰。
#   ② 刷新桌面数据库与图标缓存——deb 没有 pacman 的自动触发器。漏掉桌面数据库
#      会让「应用列表里搜不到」；图标缓存只是性能/旧主题优化，SVG 仍能按路径加载。
#      GTK4 的工具叫 gtk4-update-icon-cache，旧发行版仍可能只有 gtk-update-icon-cache。
# 任何一步失败都不能让安装事务失败：doctor 会把环境问题如实报出来。
set -u

src=/usr/share/appclone/bwrap.apparmor
dst=/etc/apparmor.d/bwrap
tmp=/etc/apparmor.d/.appclone-bwrap.new

reload_profile() {
	parser=$(command -v apparmor_parser 2>/dev/null || true)
	[ -z "$parser" ] && [ -x /usr/sbin/apparmor_parser ] && parser=/usr/sbin/apparmor_parser
	if [ -n "$parser" ]; then
		# parser 失败时删掉目标，让下次安装/重启重试，而不是留下一个
		# 看起来"已安装"、实际未加载的 profile。
		"$parser" -r "$dst" >/dev/null 2>&1 || rm -f "$dst" 2>/dev/null || true
	fi
}

# 全新安装：只在系统确实启用了 userns 限制时才装。
install_fresh() {
	if cp "$src" "$tmp" 2>/dev/null && chmod 0644 "$tmp" 2>/dev/null && mv -f "$tmp" "$dst" 2>/dev/null; then
		reload_profile
	fi
	rm -f "$tmp" 2>/dev/null || true
}

# 升级迁移：目标已存在且无标记。内容与模板一致（忽略标记行）说明它是旧版
# appclone 写的；用带标记的模板覆盖即可，策略内容没变，不需要重新加载。
migrate_legacy() {
	old=/etc/apparmor.d/.appclone-bwrap.old
	cur=/etc/apparmor.d/.appclone-bwrap.cur
	if grep -v '^# appclone-owned:' "$src" >"$old" 2>/dev/null &&
		grep -v '^# appclone-owned:' "$dst" >"$cur" 2>/dev/null &&
		cmp -s "$old" "$cur"; then
		if cp "$src" "$tmp" 2>/dev/null && chmod 0644 "$tmp" 2>/dev/null && mv -f "$tmp" "$dst" 2>/dev/null; then
			:
		fi
	fi
	rm -f "$tmp" "$old" "$cur" 2>/dev/null || true
}

if [ -r "$src" ]; then
	if [ ! -e "$dst" ]; then
		restrict=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo 0)
		if [ "$restrict" = "1" ]; then
			install_fresh
		fi
	elif ! grep -q '^# appclone-owned:' "$dst" 2>/dev/null; then
		migrate_legacy
	fi
fi

command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database -q >/dev/null 2>&1 || true
if command -v gtk4-update-icon-cache >/dev/null 2>&1; then
	gtk4-update-icon-cache -q -t -f /usr/share/icons/hicolor >/dev/null 2>&1 || true
elif command -v gtk-update-icon-cache >/dev/null 2>&1; then
	gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor >/dev/null 2>&1 || true
fi

exit 0
