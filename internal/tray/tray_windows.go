//go:build windows

package tray

import (
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pRegisterClassEx        = user32.NewProc("RegisterClassExW")
	pCreateWindowEx         = user32.NewProc("CreateWindowExW")
	pDefWindowProc          = user32.NewProc("DefWindowProcW")
	pGetMessage             = user32.NewProc("GetMessageW")
	pTranslateMessage       = user32.NewProc("TranslateMessage")
	pDispatchMessage        = user32.NewProc("DispatchMessageW")
	pPostQuitMessage        = user32.NewProc("PostQuitMessage")
	pDestroyWindow          = user32.NewProc("DestroyWindow")
	pCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	pAppendMenu             = user32.NewProc("AppendMenuW")
	pTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	pDestroyMenu            = user32.NewProc("DestroyMenu")
	pSetMenuDefaultItem     = user32.NewProc("SetMenuDefaultItem")
	pSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	pGetCursorPos           = user32.NewProc("GetCursorPos")
	pPostMessage            = user32.NewProc("PostMessageW")
	pRegisterWindowMessage  = user32.NewProc("RegisterWindowMessageW")
	pCreateIconIndirect     = user32.NewProc("CreateIconIndirect")
	pDestroyIcon            = user32.NewProc("DestroyIcon")
	pGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	pLoadCursor             = user32.NewProc("LoadCursorW")
	pGetModuleHandle        = kernel32.NewProc("GetModuleHandleW")
	pShellNotifyIcon        = shell32.NewProc("Shell_NotifyIconW")
	pCreateDIBSection       = gdi32.NewProc("CreateDIBSection")
	pCreateBitmap           = gdi32.NewProc("CreateBitmap")
	pDeleteObject           = gdi32.NewProc("DeleteObject")
)

const (
	wmNull        = 0x0000
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmCommand     = 0x0111
	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmContextMenu = 0x007B
	wmApp         = 0x8000
	wmTrayIcon    = wmApp + 1
	wmTrayUpdate  = wmApp + 2

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10

	niifInfo    = 0x1
	niifWarning = 0x2
	niifError   = 0x3

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfSeparator = 0x0800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
	tpmNoNotify    = 0x0080

	smCxSmIcon = 49
	idcArrow   = 32512
)

const (
	cmdHeader = 1000 + iota
	cmdOpenDashboard
	cmdCopyProxies
	cmdStartAll
	cmdStopAll
	cmdRestartFailed
	cmdNewIdentity
	cmdExit
)

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     uintptr
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type msgT struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	_       uint32
}

type point struct{ X, Y int32 }

type bitmapInfoHeader struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

type balloon struct {
	title, text string
	flags       uint32
}

// Tray is a running notification-area icon. All methods are goroutine-safe.
type Tray struct {
	cb Callbacks

	mu       sync.Mutex
	hwnd     uintptr
	tooltip  string
	status   Status
	menu     MenuState
	balloons []balloon
	closed   bool

	icons      map[Status]uintptr
	taskbarMsg uint32
	added      bool
	done       chan struct{}
}

var (
	activeMu  sync.Mutex
	active    *Tray
	classOnce sync.Once
	classErr  error
	className = windows.StringToUTF16Ptr("TorProxyManagerTrayWnd")
)

// New creates the tray icon and starts its message loop on a dedicated OS thread.
func New(tooltip string, cb Callbacks) (*Tray, error) {
	activeMu.Lock()
	if active != nil {
		activeMu.Unlock()
		return nil, fmt.Errorf("tray: already created")
	}
	t := &Tray{
		cb:      cb,
		tooltip: tooltip,
		status:  StatusStopped,
		menu:    MenuState{CanStart: true},
		icons:   make(map[Status]uintptr),
		done:    make(chan struct{}),
	}
	active = t
	activeMu.Unlock()

	ready := make(chan error, 1)
	go t.loop(ready)
	if err := <-ready; err != nil {
		activeMu.Lock()
		active = nil
		activeMu.Unlock()
		return nil, err
	}
	return t, nil
}

func (t *Tray) loop(ready chan<- error) {
	// Win32 windows belong to the thread that created them; the message loop
	// must run on that same thread for the lifetime of the window.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(t.done)

	hInst, _, _ := pGetModuleHandle.Call(0)
	classOnce.Do(func() {
		cursor, _, _ := pLoadCursor.Call(0, idcArrow)
		wc := wndClassEx{
			LpfnWndProc:   windows.NewCallback(wndProc),
			HInstance:     hInst,
			HCursor:       cursor,
			LpszClassName: className,
		}
		wc.CbSize = uint32(unsafe.Sizeof(wc))
		if r, _, err := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			classErr = fmt.Errorf("tray: RegisterClassEx: %v", err)
		}
	})
	if classErr != nil {
		ready <- classErr
		return
	}

	tbName := windows.StringToUTF16Ptr("TaskbarCreated")
	m, _, _ := pRegisterWindowMessage.Call(uintptr(unsafe.Pointer(tbName)))
	t.taskbarMsg = uint32(m)

	title := windows.StringToUTF16Ptr("TorProxyManager")
	hwnd, _, err := pCreateWindowEx.Call(0,
		uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if hwnd == 0 {
		ready <- fmt.Errorf("tray: CreateWindowEx: %v", err)
		return
	}
	t.mu.Lock()
	t.hwnd = hwnd
	t.mu.Unlock()

	if !t.addIcon() {
		pDestroyWindow.Call(hwnd)
		ready <- fmt.Errorf("tray: Shell_NotifyIcon(NIM_ADD) failed")
		return
	}
	ready <- nil

	var msg msgT
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT, -1 = error
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}

	for _, h := range t.icons {
		pDestroyIcon.Call(h)
	}
	activeMu.Lock()
	if active == t {
		active = nil
	}
	activeMu.Unlock()
}

// --- public API ---

// SetStatus changes the icon tint and tooltip text.
func (t *Tray) SetStatus(s Status, tooltip string) {
	t.mu.Lock()
	t.status = s
	if tooltip != "" {
		t.tooltip = tooltip
	}
	t.mu.Unlock()
	t.post(wmTrayUpdate)
}

// SetMenuState updates which context-menu actions are enabled.
func (t *Tray) SetMenuState(ms MenuState) {
	t.mu.Lock()
	t.menu = ms
	t.mu.Unlock()
}

// Notify shows a balloon / toast notification. level: 0 info, 1 warning, 2 error.
func (t *Tray) Notify(title, text string, level int) {
	flags := uint32(niifInfo)
	switch level {
	case 1:
		flags = niifWarning
	case 2:
		flags = niifError
	}
	t.mu.Lock()
	t.balloons = append(t.balloons, balloon{title, text, flags})
	t.mu.Unlock()
	t.post(wmTrayUpdate)
}

// Close removes the icon and stops the message loop. Safe to call repeatedly.
func (t *Tray) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.mu.Unlock()
	t.post(wmClose)
	select {
	case <-t.done:
	case <-time.After(3 * time.Second):
	}
}

func (t *Tray) post(msg uint32) {
	t.mu.Lock()
	h := t.hwnd
	t.mu.Unlock()
	if h != 0 {
		pPostMessage.Call(h, uintptr(msg), 0, 0)
	}
}

// --- GUI-thread helpers ---

func (t *Tray) baseNID() notifyIconData {
	nid := notifyIconData{HWnd: t.hwnd, UID: 1}
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	return nid
}

func (t *Tray) addIcon() bool {
	t.mu.Lock()
	st, tip := t.status, t.tooltip
	t.mu.Unlock()
	nid := t.baseNID()
	nid.UFlags = nifMessage | nifIcon | nifTip
	nid.UCallbackMessage = wmTrayIcon
	nid.HIcon = t.icon(st)
	copyUTF16(nid.SzTip[:], tip)
	r, _, _ := pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	t.added = r != 0
	return t.added
}

func (t *Tray) applyUpdate() {
	t.mu.Lock()
	st, tip := t.status, t.tooltip
	pending := t.balloons
	t.balloons = nil
	t.mu.Unlock()

	nid := t.baseNID()
	nid.UFlags = nifIcon | nifTip
	nid.HIcon = t.icon(st)
	copyUTF16(nid.SzTip[:], tip)
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid)))

	for _, b := range pending {
		n := t.baseNID()
		n.UFlags = nifInfo
		n.DwInfoFlags = b.flags
		copyUTF16(n.SzInfoTitle[:], b.title)
		copyUTF16(n.SzInfo[:], b.text)
		pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&n)))
	}
}

func (t *Tray) removeIcon() {
	if t.added {
		nid := t.baseNID()
		pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
		t.added = false
	}
}

func (t *Tray) icon(s Status) uintptr {
	if h, ok := t.icons[s]; ok {
		return h
	}
	size, _, _ := pGetSystemMetrics.Call(smCxSmIcon)
	if size < 16 {
		size = 16
	}
	h := createIcon(int(size), RenderIcon(int(size), s))
	if h != 0 {
		t.icons[s] = h
	}
	return h
}

func createIcon(size int, rgba []byte) uintptr {
	bih := bitmapInfoHeader{
		BiWidth:    int32(size),
		BiHeight:   -int32(size), // top-down
		BiPlanes:   1,
		BiBitCount: 32,
	}
	bih.BiSize = uint32(unsafe.Sizeof(bih))
	var bits unsafe.Pointer
	hColor, _, _ := pCreateDIBSection.Call(0, uintptr(unsafe.Pointer(&bih)), 0,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hColor == 0 || bits == nil {
		return 0
	}
	defer pDeleteObject.Call(hColor)

	dst := unsafe.Slice((*byte)(bits), size*size*4)
	for i := 0; i < size*size; i++ { // RGBA -> BGRA
		dst[i*4+0] = rgba[i*4+2]
		dst[i*4+1] = rgba[i*4+1]
		dst[i*4+2] = rgba[i*4+0]
		dst[i*4+3] = rgba[i*4+3]
	}

	maskBits := make([]byte, ((size+15)/16)*2*size) // zero = use colour alpha
	hMask, _, _ := pCreateBitmap.Call(uintptr(size), uintptr(size), 1, 1, uintptr(unsafe.Pointer(&maskBits[0])))
	if hMask == 0 {
		return 0
	}
	defer pDeleteObject.Call(hMask)

	ii := iconInfo{FIcon: 1, HbmMask: hMask, HbmColor: hColor}
	h, _, _ := pCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	return h
}

func (t *Tray) showMenu() {
	t.mu.Lock()
	ms := t.menu
	cb := t.cb
	hwnd := t.hwnd
	t.mu.Unlock()

	hMenu, _, _ := pCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer pDestroyMenu.Call(hMenu)

	if ms.Header != "" {
		appendItem(hMenu, cmdHeader, ms.Header, false)
		appendSep(hMenu)
	}
	if cb.OnOpenDashboard != nil {
		appendItem(hMenu, cmdOpenDashboard, "Open Dashboard", true)
		pSetMenuDefaultItem.Call(hMenu, cmdOpenDashboard, 0)
	}
	if cb.OnCopyProxies != nil {
		appendItem(hMenu, cmdCopyProxies, "Copy Proxy List", ms.HasRunning)
	}
	appendSep(hMenu)
	if cb.OnStartAll != nil {
		appendItem(hMenu, cmdStartAll, "Start All", ms.CanStart)
	}
	if cb.OnStopAll != nil {
		appendItem(hMenu, cmdStopAll, "Stop All", ms.CanStop)
	}
	if cb.OnRestartFailed != nil {
		appendItem(hMenu, cmdRestartFailed, "Restart Failed", ms.HasFailed)
	}
	if cb.OnNewIdentity != nil {
		appendItem(hMenu, cmdNewIdentity, "New Identity (All)", ms.HasRunning)
	}
	appendSep(hMenu)
	if cb.OnExit != nil {
		appendItem(hMenu, cmdExit, "Exit", true)
	}

	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Required so the menu closes when the user clicks elsewhere.
	pSetForegroundWindow.Call(hwnd)
	cmd, _, _ := pTrackPopupMenu.Call(hMenu, tpmRightButton|tpmReturnCmd|tpmNoNotify,
		uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	pPostMessage.Call(hwnd, wmNull, 0, 0)

	run := func(f func()) {
		if f != nil {
			go f()
		}
	}
	switch cmd {
	case cmdOpenDashboard:
		run(cb.OnOpenDashboard)
	case cmdCopyProxies:
		run(cb.OnCopyProxies)
	case cmdStartAll:
		run(cb.OnStartAll)
	case cmdStopAll:
		run(cb.OnStopAll)
	case cmdRestartFailed:
		run(cb.OnRestartFailed)
	case cmdNewIdentity:
		run(cb.OnNewIdentity)
	case cmdExit:
		run(cb.OnExit)
	}
}

func appendItem(hMenu uintptr, id int, text string, enabled bool) {
	flags := uintptr(mfString)
	if !enabled {
		flags |= mfGrayed
	}
	p := windows.StringToUTF16Ptr(text)
	pAppendMenu.Call(hMenu, flags, uintptr(id), uintptr(unsafe.Pointer(p)))
}

func appendSep(hMenu uintptr) { pAppendMenu.Call(hMenu, mfSeparator, 0, 0) }

func copyUTF16(dst []uint16, s string) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return
	}
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}

func wndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	activeMu.Lock()
	t := active
	activeMu.Unlock()

	if t != nil {
		switch {
		case msg == wmTrayIcon:
			switch uint32(lParam) & 0xffff {
			case wmLButtonUp:
				if t.cb.OnOpenDashboard != nil {
					go t.cb.OnOpenDashboard()
				}
			case wmRButtonUp, wmContextMenu:
				t.showMenu()
			}
			return 0
		case msg == wmTrayUpdate:
			t.applyUpdate()
			return 0
		case t.taskbarMsg != 0 && msg == t.taskbarMsg:
			// Explorer restarted: the icon must be re-added.
			t.added = false
			t.addIcon()
			return 0
		case msg == wmClose:
			t.removeIcon()
			pDestroyWindow.Call(hwnd)
			return 0
		case msg == wmDestroy:
			pPostQuitMessage.Call(0)
			return 0
		}
	}
	r, _, _ := pDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
	return r
}
