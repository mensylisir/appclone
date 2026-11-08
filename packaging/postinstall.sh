#!/bin/sh
# appclone 安装后动作（deb / rpm / Arch 共用）：
#   ① Ubuntu 23.10+ 的 AppArmor userns 限制：只给 bwrap 装一条 unconfined profile，
#      不改全局 sysctl；目标文件已存在则跳过（让位给发行版自己的版本）。
#      先写临时文件再 rename，安装中断不会留下半截 profile。
#   ② 刷新桌面数据库与图标缓存——deb 没有 pacman 的自动触发器。漏掉桌面数据库
#      会让「应用列表里搜不到」；图标缓存只是性能/旧主题优化，SVG 仍能按路径加载。
#      GTK4 的工具叫 gtk4-update-icon-cache，旧发行版仍可能只有 gtk-update-icon-cache。
# 任何一步失败都不能让安装事务失败：doctor 会把环境问题如实报出来。
set -u

src=/usr/share/appclone/bwrap.apparmor
dst=/etc/apparmor.d/bwrap

if [ -r "$src" ] && [ ! -e "$dst" ]; then
	restrict=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo 0)
	if [ "$restrict" = "1" ]; then
		tmp="/etc/apparmor.d/.appclone-bwrap.new"
		if cp "$src" "$tmp" 2>/dev/null && chmod 0644 "$tmp" 2>/dev/null; then
			if mv -f "$tmp" "$dst" 2>/dev/null; then
				# parser 失败时删掉目标，让下次安装/重启重试，而不是留下一个
				# 看起来"已安装"、实际未加载的 profile。
				parser=$(command -v apparmor_parser 2>/dev/null || true)
				[ -z "$parser" ] && [ -x /usr/sbin/apparmor_parser ] && parser=/usr/sbin/apparmor_parser
				if [ -n "$parser" ]; then
					"$parser" -r "$dst" >/dev/null 2>&1 || rm -f "$dst" 2>/dev/null || true
				fi
			fi
		fi
		rm -f "$tmp" 2>/dev/null || true
	fi
fi

command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database -q >/dev/null 2>&1 || true
if command -v gtk4-update-icon-cache >/dev/null 2>&1; then
	gtk4-update-icon-cache -q -t -f /usr/share/icons/hicolor >/dev/null 2>&1 || true
elif command -v gtk-update-icon-cache >/dev/null 2>&1; then
	gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor >/dev/null 2>&1 || true
fi

exit 0
