package core

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// moveRename 是 os.Rename 的间接层：单测无法真的制造 EXDEV，只能替换它
// 来走跨设备回退。生产路径永远指向 os.Rename。
var moveRename = os.Rename

// moveDir 把整个目录搬到 dst。同文件系统用 rename；跨文件系统（EXDEV）退化为
// 拷贝 + 删除源。
//
// 为什么不能只写 os.Rename：审计写层固定在 /var/tmp，实例目录在 $HOME，
// 而 /home 独立分区是普通桌面配置。rename 跨设备直接 EXDEV，leak 结论就永远
// 落不了地——证据还在，但每次 verify 都报错。
//
// **任何失败都不删源**：搬的是可能属于用户的文件。只有拷贝完整（含目录项
// 落盘）成功后，才允许删除源目录；拷贝中途失败时只回收 dst 这个刚写出的半成品。
// dst 已存在直接失败：绝不先删一个我们没建过的目录——那可能是别人正在用的证据。
func moveDir(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("moveDir: destination exists: %s", dst)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := moveRename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyTree(src, dst); err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	// 目录项也要落盘：只 fsync 文件内容时，断电窗口里可能出现"源已删、
	// 目标目录项还没写下去"——那是真的丢证据。sync 失败时源仍在，回收 dst 重试。
	if err := syncTree(dst); err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	return os.RemoveAll(src)
}

// copyTree 递归拷贝目录，保留普通文件内容与权限、保留软链。
//
// overlay 的 whiteout 标记（字符设备 0,0）无法在非特权下重建，也不是用户数据：
// 直接跳过。它们只表示"某个路径被删过"，不承载任何可恢复内容。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type()&fs.ModeSymlink != 0:
			link, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			return copyFile(path, target)
		default:
			// 设备/管道/socket：不是用户文件，跳过。
			return nil
		}
	})
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	// O_NOFOLLOW/O_NONBLOCK：WalkDir 判定普通文件与真正打开之间有窗口，
	// 同 uid 进程把它换成 FIFO 时，裸 Open 会永久阻塞（"禁止死等"）。
	in, err := os.OpenFile(src, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	// 证据要能挺过断电：先落盘再删源，否则崩溃窗口里两边都没有。
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// syncTree 把目录树里每个目录项本身落盘（文件内容在 copyFile 里已 fsync）。
func syncTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if !d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		ferr := f.Sync()
		cerr := f.Close()
		if ferr != nil {
			return ferr
		}
		return cerr
	})
}

// removeAuditWork 清掉审计的临时层（work/merged）。upper 还在就不动：
// 那是证据，删不删由 verify 的结论决定。
func (e *Env) removeAuditWork(app, id string) {
	work := e.AuditWorkDir(app, id)
	if _, err := os.Stat(filepath.Join(work, "upper")); err == nil {
		return
	}
	_ = os.RemoveAll(work)
}
