//go:build windows

package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// The icon is also used by the Wails build as build/appicon.png. Keeping an
// ICO beside the source lets the tray use the same visual without depending
// on a third-party notification-area package.
//
//go:embed assets/app.ico
var trayIconBytes []byte

const (
	wmApp          = 0x8000
	wmTrayMessage  = wmApp + 1
	wmTrayExit     = wmApp + 2
	wmLButtonUp    = 0x0202
	wmLButtonDbl   = 0x0203
	wmRButtonUp    = 0x0205
	wmDestroy      = 0x0002
	wsExToolWindow = 0x00000080
	wsPopup        = 0x80000000

	nimAdd     = 0x00000000
	nimDelete  = 0x00000002
	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	imageIcon      = 1
	lrDefaultSize  = 0x00000040
	lrLoadFromFile = 0x00000010

	mfString       = 0x00000000
	mfSeparator    = 0x00000800
	tpmRightButton = 0x00000002
	tpmBottomAlign = 0x00000020
	tpmReturnCmd   = 0x00000100

	trayMenuOpen = 1001
	trayMenuExit = 1002
)

var (
	trayUser32  = syscall.NewLazyDLL("user32.dll")
	trayShell32 = syscall.NewLazyDLL("shell32.dll")
	trayKernel  = syscall.NewLazyDLL("kernel32.dll")

	trayRegisterClassEx = trayUser32.NewProc("RegisterClassExW")
	trayUnregisterClass = trayUser32.NewProc("UnregisterClassW")
	trayCreateWindowEx  = trayUser32.NewProc("CreateWindowExW")
	trayDestroyWindow   = trayUser32.NewProc("DestroyWindow")
	trayDefWindowProc   = trayUser32.NewProc("DefWindowProcW")
	trayGetMessage      = trayUser32.NewProc("GetMessageW")
	trayTranslate       = trayUser32.NewProc("TranslateMessage")
	trayDispatch        = trayUser32.NewProc("DispatchMessageW")
	trayPostMessage     = trayUser32.NewProc("PostMessageW")
	trayPostQuit        = trayUser32.NewProc("PostQuitMessage")
	trayGetCursorPos    = trayUser32.NewProc("GetCursorPos")
	traySetForeground   = trayUser32.NewProc("SetForegroundWindow")
	trayCreateMenu      = trayUser32.NewProc("CreatePopupMenu")
	trayAppendMenu      = trayUser32.NewProc("AppendMenuW")
	trayTrackMenu       = trayUser32.NewProc("TrackPopupMenu")
	trayDestroyMenu     = trayUser32.NewProc("DestroyMenu")
	trayLoadImage       = trayUser32.NewProc("LoadImageW")
	trayDestroyIcon     = trayUser32.NewProc("DestroyIcon")
	trayGetModuleHandle = trayKernel.NewProc("GetModuleHandleW")
	trayShellNotify     = trayShell32.NewProc("Shell_NotifyIconW")
)

type trayPoint struct {
	x int32
	y int32
}

type trayMessage struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	point   trayPoint
}

type trayGuid struct {
	data1 uint32
	data2 uint16
	data3 uint16
	data4 [8]byte
}

type trayNotifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uTimeoutVersion  uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         trayGuid
	hBalloonIcon     uintptr
}

type trayWndClassEx struct {
	cbSize      uint32
	style       uint32
	windowProc  uintptr
	classExtra  int32
	windowExtra int32
	instance    uintptr
	icon        uintptr
	cursor      uintptr
	background  uintptr
	menuName    *uint16
	className   *uint16
	smallIcon   uintptr
}

type trayController struct {
	ctx       context.Context
	mu        sync.Mutex
	hwnd      uintptr
	className *uint16
	instance  uintptr
	icon      uintptr
	ready     chan struct{}
	done      chan struct{}
	readyOnce sync.Once
}

func (a *App) startTray(ctx context.Context) {
	controller := &trayController{
		ctx:   ctx,
		ready: make(chan struct{}),
		done:  make(chan struct{}),
	}
	a.mu.Lock()
	a.trayStop = controller.stop
	a.mu.Unlock()
	go controller.run()
}

func (a *App) stopTray() {
	a.mu.Lock()
	stop := a.trayStop
	a.trayStop = nil
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (t *trayController) signalReady() {
	t.readyOnce.Do(func() { close(t.ready) })
}

func (t *trayController) stop() {
	select {
	case <-t.ready:
	case <-time.After(2 * time.Second):
		return
	}
	t.mu.Lock()
	hwnd := t.hwnd
	t.mu.Unlock()
	if hwnd != 0 {
		_, _, _ = trayPostMessage.Call(hwnd, wmTrayExit, 0, 0)
	}
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
	}
}

func (t *trayController) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(t.done)

	instance, _, _ := trayGetModuleHandle.Call(0)
	t.instance = instance
	className, err := syscall.UTF16PtrFromString(fmt.Sprintf("GBFLocalCacheTray-%d", os.Getpid()))
	if err != nil {
		t.signalReady()
		return
	}
	t.className = className
	windowProc := syscall.NewCallback(t.windowProc)
	class := trayWndClassEx{
		cbSize:     uint32(unsafe.Sizeof(trayWndClassEx{})),
		windowProc: windowProc,
		instance:   instance,
		className:  className,
	}
	if atom, _, _ := trayRegisterClassEx.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		t.signalReady()
		return
	}
	defer trayUnregisterClass.Call(uintptr(unsafe.Pointer(className)), instance)

	hwnd, _, _ := trayCreateWindowEx.Call(
		wsExToolWindow,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(className)),
		wsPopup,
		0, 0, 0, 0,
		0, 0, instance, 0,
	)
	if hwnd == 0 {
		t.signalReady()
		return
	}
	t.mu.Lock()
	t.hwnd = hwnd
	t.mu.Unlock()
	defer func() {
		if t.icon != 0 {
			trayDestroyIcon.Call(t.icon)
		}
		trayDestroyWindow.Call(hwnd)
		t.mu.Lock()
		t.hwnd = 0
		t.mu.Unlock()
	}()

	icon, iconErr := t.loadIcon()
	if iconErr == nil {
		t.icon = icon
	}
	notify := trayNotifyIconData{
		cbSize:           uint32(unsafe.Sizeof(trayNotifyIconData{})),
		hWnd:             hwnd,
		uID:              1,
		uFlags:           nifMessage | nifTip,
		uCallbackMessage: wmTrayMessage,
	}
	if t.icon != 0 {
		notify.uFlags |= nifIcon
		notify.hIcon = t.icon
	}
	if tip, tipErr := syscall.UTF16FromString("GBF Local Cache"); tipErr == nil {
		copy(notify.szTip[:], tip)
	}
	trayShellNotify.Call(nimAdd, uintptr(unsafe.Pointer(&notify)))
	t.signalReady()
	defer func() {
		notify.uFlags = 0
		trayShellNotify.Call(nimDelete, uintptr(unsafe.Pointer(&notify)))
	}()

	var message trayMessage
	for {
		result, _, _ := trayGetMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(result) <= 0 {
			return
		}
		trayTranslate.Call(uintptr(unsafe.Pointer(&message)))
		trayDispatch.Call(uintptr(unsafe.Pointer(&message)))
	}
}

func (t *trayController) loadIcon() (uintptr, error) {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("gbf-local-cache-%d.ico", os.Getpid()))
	if err := os.WriteFile(path, trayIconBytes, 0o600); err != nil {
		return 0, err
	}
	defer os.Remove(path)
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	icon, _, callErr := trayLoadImage.Call(0, uintptr(unsafe.Pointer(name)), imageIcon, 32, 32, lrDefaultSize|lrLoadFromFile)
	if icon == 0 {
		return 0, callErr
	}
	return icon, nil
}

func (t *trayController) windowProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmTrayMessage:
		switch uint32(lParam) {
		case wmLButtonUp, wmLButtonDbl, wmRButtonUp:
			if uint32(lParam) == wmRButtonUp {
				t.showMenu()
			} else {
				wailsruntime.WindowShow(t.ctx)
			}
		}
		return 0
	case wmTrayExit:
		trayPostQuit.Call(0)
		return 0
	case wmDestroy:
		return 0
	default:
		result, _, _ := trayDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
		return result
	}
}

func (t *trayController) showMenu() {
	var point trayPoint
	trayGetCursorPos.Call(uintptr(unsafe.Pointer(&point)))
	menu, _, _ := trayCreateMenu.Call()
	if menu == 0 {
		return
	}
	defer trayDestroyMenu.Call(menu)
	openText, _ := syscall.UTF16PtrFromString("打开控制面板")
	exitText, _ := syscall.UTF16PtrFromString("退出 GBF Local Cache")
	trayAppendMenu.Call(menu, mfString, trayMenuOpen, uintptr(unsafe.Pointer(openText)))
	trayAppendMenu.Call(menu, mfSeparator, 0, 0)
	trayAppendMenu.Call(menu, mfString, trayMenuExit, uintptr(unsafe.Pointer(exitText)))
	traySetForeground.Call(t.hwnd)
	command, _, _ := trayTrackMenu.Call(menu, tpmRightButton|tpmBottomAlign|tpmReturnCmd, uintptr(point.x), uintptr(point.y), 0, t.hwnd, 0)
	trayPostMessage.Call(t.hwnd, 0, 0, 0)
	switch command {
	case trayMenuOpen:
		wailsruntime.WindowShow(t.ctx)
	case trayMenuExit:
		wailsruntime.Quit(t.ctx)
	}
}
