package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// startSessionBus 等 --print-address 必须有界：daemon 卡住不输出时，
// 整个 run 会停在 setup，信号泵拿不到 pgid，分身变成"停不掉也起不来"。
func TestSessionBusAddressTimeout(t *testing.T) {
	e := testEnv(t)
	seedInstance(t, e)
	in, err := e.GetInstance("testapp", "main")
	if err != nil {
		t.Fatal(err)
	}

	// 假 daemon：exec 成 sleep，Kill 直接打在这个进程上，不会留下孤儿。
	dir := t.TempDir()
	fake := filepath.Join(dir, "dbus-daemon")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	begin := time.Now()
	_, err = e.startSessionBus(filepath.Join(in.Dir(), "dbus"))
	elapsed := time.Since(begin)

	var ce *Error
	if !errors.As(err, &ce) || ce.Key != "err.env.dbus_timeout" {
		t.Fatalf("地址读取超时应报 err.env.dbus_timeout，得到 %v", err)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("超时应在约 2 秒内返回，实际 %v", elapsed)
	}
}
