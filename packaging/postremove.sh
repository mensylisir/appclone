#!/bin/sh
# appclone 卸载后动作（deb / rpm / Arch 共用）：回收自己安装的 AppArmor profile。
#
# 只删带 `appclone-owned:` 标记的那份：postinstall 在目标不存在时安装，或在
# 目标是无标记的 v1.0.0 旧文件时补写标记；若之后发行版/其他包用同名文件替换
# 了它，标记随之消失，这里一律不碰。
#
# 删除前尽力让内核忘掉 profile；卸载失败也仍然删文件——文件留着会在下次开机
# 被重新加载，等于 bwrap 永久保持 unconfined，比"本次开机残留"更糟。包删除
# 本身必须成功，所有错误都吞掉并 exit 0。
set -u

# 升级不是卸载。deb 的旧 postrm 会收到 upgrade；rpm 升级时旧包的 %postun 会在
# 新包 %post 之后执行（参数为 1）。此时若删文件，会把新版刚装好的 profile 干掉。
# Arch 的 post_remove 只在 alpm 判定 newpkg == NULL 时执行（参数是旧包版本号），
# 升级不会走到这里；上面的守卫覆盖 deb/rpm。
case "${1:-}" in
1 | upgrade | failed-upgrade | abort-upgrade) exit 0 ;;
esac

dst=/etc/apparmor.d/bwrap
[ -f "$dst" ] || exit 0
grep -q '^# appclone-owned:' "$dst" 2>/dev/null || exit 0

parser=$(command -v apparmor_parser 2>/dev/null || true)
[ -z "$parser" ] && [ -x /usr/sbin/apparmor_parser ] && parser=/usr/sbin/apparmor_parser
if [ -n "$parser" ]; then
	"$parser" -R "$dst" >/dev/null 2>&1 || true
fi
rm -f "$dst" 2>/dev/null || true

exit 0
