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
// 为什么不能只写 os.Rename：审计写层在 /tmp（ADR-033），实例目录在 $HOME，
// 而 /home 独立分区是普通桌面配置。rename 跨设备直接 EXDEV，leak 结论就永远
// 落不了地——证据还在，但每次 verify 都报错。
//
// **拷贝完成前绝不动源**：搬的是可能属于用户的文件。拷贝中途失败时只回收
// dst 这个刚写出的半成品，源原样留着。拷贝 + fsync 都成功后才会删源；删源
// 失败（子目录 0555 之类）不代表证据丢了——dst 已是完整副本，返回
// *dstCompleteError 让调用方按"已搬走"处理，别把用户引向残缺的另一份。
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
	if err := os.RemoveAll(src); err != nil {
		// 常见原因是应用自己把子目录改成 0555：我们对副本有完整控制权，
		// 先在源树上补写权限再删一次。仍删不掉才交给调用方按"已搬走"处理。
		if werr := makeWritableAndRemove(src); werr == nil {
			return nil
		}
		return &dstCompleteError{dst: dst, err: err}
	}
	return nil
}

// removeChmod 只为单测替换：制造"连 chmod 都不允许"的源，验证 dstComplete 分支。
var removeChmod = os.Chmod

func makeWritableAndRemove(root string) error {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil // 读不到的条目照样尝试 chmod：父目录先放权就可能读到
		}
		if d.IsDir() {
			_ = removeChmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// dstCompleteError 表示"副本已完整落盘，只是源没删干净"。它不是搬迁失败：
// 调用方必须按成功处理（证据在 dst），残留的源交给下一次 sweep。
type dstCompleteError struct {
	dst string
	err error
}

func (e *dstCompleteError) Error() string {
	return fmt.Sprintf("moveDir: destination complete (%s), source not fully removed: %v", e.dst, e.err)
}

func (e *dstCompleteError) Unwrap() error { return e.err }

// dstComplete 判断 moveDir 的返回值是否只是"源删不干净"。
func dstComplete(err error) bool {
	var dc *dstCompleteError
	return errors.As(err, &dc)
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
