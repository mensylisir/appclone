#!/bin/sh
# appclone 卸载后动作（deb / rpm / Arch 共用）：回收自己安装的 AppArmor profile。
#
# 只删带 `appclone-owned:` 标记的那份：postinstall 只在目标文件不存在时才复制，
# 若之后发行版/其他包用同名文件替换了它，标记随之消失，这里一律不碰。
# 删除前先让内核忘掉 profile；卸载失败就保留文件（包删除本身仍必须成功），
# 由下次安装或发行版升级再处理——残留的 unconfined profile 不影响系统。
set -u

# 升级不是卸载：rpm 升级时旧包的 %postun 会在新包 %post 之后执行（参数为 1），
# deb 的旧 postrm 会收到 upgrade。此时若删文件，会把新版刚装好的 profile 干掉。
# Arch 的 post_remove 只在真正移除时执行，不带参数。
case "${1:-}" in
1 | upgrade | failed-upgrade | abort-upgrade) exit 0 ;;
esac

dst=/etc/apparmor.d/bwrap
[ -f "$dst" ] || exit 0
grep -q '^# appclone-owned:' "$dst" 2>/dev/null || exit 0

parser=$(command -v apparmor_parser 2>/dev/null || true)
[ -z "$parser" ] && [ -x /usr/sbin/apparmor_parser ] && parser=/usr/sbin/apparmor_parser
if [ -n "$parser" ]; then
	"$parser" -R "$dst" >/dev/null 2>&1 || exit 0
fi
rm -f "$dst" 2>/dev/null || true

exit 0
