//go:build linux && gtk_e2e

package main

// uinput 真实输入注入（Wayland 原生，无需 root）。
//
// 测试进程通过 /dev/uinput 创建两个虚拟设备：一个单点触摸屏（绝对坐标点击）
// 和一个键盘（扫描码）。内核把事件投进当前 seat，GNOME/KWin 用 libinput 收到后
// 按普通输入设备处理——这是 ydotool 走的那条路，与 X11/XTEST 无关，因此在
// Wayland 会话里同样有效。
//
// 边界与安全：
//   - 注入会进入用户当前会话。用例只有在窗口 IsActive 后才注入，并且在每次
//     注入前复查焦点；窗口不是全屏或焦点不在本窗口就安全中止，绝不把按键送进
//     别的应用。
//   - 只有用例自己打开全屏窗口时才用触摸坐标：全屏窗口的 surface 原点就是
//     显示器原点，控件坐标 = 屏幕坐标，不需要（Wayland 也不提供）全局窗口位置。
//   - 触摸屏的 ABS 范围映射到所有显示器组成的包围盒（实测行为），所以这里按
//     显示器几何的并集换算，而不是只按主显示器。
//   - 设备 fd 关闭时 uinput 自动销毁设备；测试失败也不会留下虚拟设备。

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	uiSetEvBit   = 0x40045564
	uiSetKeyBit  = 0x40045565
	uiSetAbsBit  = 0x40045567
	uiSetPropBit = 0x4004556e
	uiDevSetup   = 0x405c5503
	uiAbsSetup   = 0x401c5504
	uiDevCreate  = 0x5501
	uiDevDestroy = 0x5502

	evSyn     = 0
	evKey     = 1
	evAbs     = 3
	synReport = 0

	btnTouch = 0x14a

	absX            = 0x00
	absY            = 0x01
	absMTSlot       = 0x2f
	absMTPositionX  = 0x35
	absMTPositionY  = 0x36
	absMTTrackingID = 0x39

	inputPropDirect = 1

	// 触摸屏 ABS 常用满量程；映射时按包围盒线性换算。
	absMax = 32767
)

type uinputID struct {
	BusType, Vendor, Product, Version uint16
}

type uinputSetupStruct struct {
	ID           uinputID
	Name         [80]byte
	FFEffectsMax uint32
}

type uinputAbsInfo struct {
	Value, Minimum, Maximum, Fuzz, Flat, Resolution int32
}

type uinputAbsSetupStruct struct {
	Code    uint16
	_       uint16
	AbsInfo uinputAbsInfo
}

type uinputEvent struct {
	Sec, Usec  int64
	Type, Code uint16
	Value      int32
}

// uinputDevice 是一个已创建的虚拟设备；写事件时统一走 emit。
type uinputDevice struct {
	f *os.File
}

func ioctlInt(fd uintptr, req uintptr, v uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, v); errno != 0 {
		return errno
	}
	return nil
}

func ioctlPtr(fd uintptr, req uintptr, p unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(p)); errno != 0 {
		return errno
	}
	return nil
}

func openUinput(name string, configure func(fd uintptr) error) (*uinputDevice, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fd := f.Fd()
	if err := configure(fd); err != nil {
		f.Close()
		return nil, err
	}
	setup := uinputSetupStruct{
		ID: uinputID{BusType: 0x03 /* BUS_USB */, Vendor: 0x1234, Product: 0x5678, Version: 1},
	}
	copy(setup.Name[:], name)
	if err := ioctlPtr(fd, uiDevSetup, unsafe.Pointer(&setup)); err != nil {
		f.Close()
		return nil, fmt.Errorf("UI_DEV_SETUP: %w", err)
	}
	if err := ioctlInt(fd, uiDevCreate, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("UI_DEV_CREATE: %w", err)
	}
	// udev/libinput 需要一点时间认识新设备；太早注入会被丢掉。
	time.Sleep(400 * time.Millisecond)
	return &uinputDevice{f: f}, nil
}

func absSetup(fd uintptr, code uint16, max int32) error {
	s := uinputAbsSetupStruct{Code: code, AbsInfo: uinputAbsInfo{Minimum: 0, Maximum: max}}
	return ioctlPtr(fd, uiAbsSetup, unsafe.Pointer(&s))
}

func newTouchscreen() (*uinputDevice, error) {
	return openUinput("appclone-gtk-e2e-touch", func(fd uintptr) error {
		for _, bit := range []uintptr{evKey, evAbs} {
			if err := ioctlInt(fd, uiSetEvBit, bit); err != nil {
				return err
			}
		}
		if err := ioctlInt(fd, uiSetKeyBit, btnTouch); err != nil {
			return err
		}
		for _, code := range []uint16{absX, absY, absMTSlot, absMTPositionX, absMTPositionY, absMTTrackingID} {
			if err := ioctlInt(fd, uiSetAbsBit, uintptr(code)); err != nil {
				return err
			}
		}
		if err := ioctlInt(fd, uiSetPropBit, inputPropDirect); err != nil {
			return err
		}
		for _, code := range []uint16{absX, absY} {
			if err := absSetup(fd, code, absMax); err != nil {
				return err
			}
		}
		for _, code := range []uint16{absMTPositionX, absMTPositionY} {
			if err := absSetup(fd, code, absMax); err != nil {
				return err
			}
		}
		return absSetup(fd, absMTSlot, 9)
	})
}

func newKeyboard(keys []uint16) (*uinputDevice, error) {
	return openUinput("appclone-gtk-e2e-kbd", func(fd uintptr) error {
		if err := ioctlInt(fd, uiSetEvBit, evKey); err != nil {
			return err
		}
		for _, code := range keys {
			if err := ioctlInt(fd, uiSetKeyBit, uintptr(code)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *uinputDevice) emit(typ, code uint16, value int32) {
	var buf [24]byte
	binary.LittleEndian.PutUint16(buf[16:], typ)
	binary.LittleEndian.PutUint16(buf[18:], code)
	binary.LittleEndian.PutUint32(buf[20:], uint32(value))
	if _, err := d.f.Write(buf[:]); err != nil {
		// 注入失败只影响本用例；不要在回调里 panic。调用方会在断言处看到状态没变。
		return
	}
}

func (d *uinputDevice) syn() { d.emit(evSyn, synReport, 0) }

// tapAt 在屏幕坐标（逻辑像素）处点一下。坐标按整块桌面（多显示器包围盒）
// 换算到触摸屏的绝对量程。
func (d *uinputDevice) tapAt(x, y float64, desktopX, desktopY, desktopW, desktopH int32) {
	if desktopW <= 0 || desktopH <= 0 {
		return
	}
	ax := int32((x - float64(desktopX)) * absMax / float64(desktopW))
	ay := int32((y - float64(desktopY)) * absMax / float64(desktopH))
	if ax < 0 {
		ax = 0
	} else if ax > absMax {
		ax = absMax
	}
	if ay < 0 {
		ay = 0
	} else if ay > absMax {
		ay = absMax
	}

	d.emit(evAbs, absMTSlot, 0)
	d.emit(evAbs, absMTTrackingID, 1)
	d.emit(evAbs, absMTPositionX, ax)
	d.emit(evAbs, absMTPositionY, ay)
	d.emit(evAbs, absX, ax)
	d.emit(evAbs, absY, ay)
	d.syn()
	d.emit(evKey, btnTouch, 1)
	d.syn()
	time.Sleep(60 * time.Millisecond)
	d.emit(evKey, btnTouch, 0)
	d.emit(evAbs, absMTTrackingID, -1)
	d.syn()
}

// pressKey 发一次按下/抬起。扫描码是 Linux input 的 KEY_* 值。
func (d *uinputDevice) pressKey(code uint16) {
	d.emit(evKey, code, 1)
	d.syn()
	time.Sleep(15 * time.Millisecond)
	d.emit(evKey, code, 0)
	d.syn()
	time.Sleep(15 * time.Millisecond)
}

func (d *uinputDevice) Close() {
	if d == nil || d.f == nil {
		return
	}
	_ = ioctlInt(d.f.Fd(), uiDevDestroy, 0)
	_ = d.f.Close()
}

// Linux input-event-codes 里本用例用到的键（US 布局）。
const (
	keyReturn    = 28
	keyBackspace = 14
	keyD         = 32
	keyE         = 18
	keyM         = 50
	keyO         = 24
)
