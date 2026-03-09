// pwaWarden - v5.18.1
package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

//go:embed assets/pwaWarden-icon.png
var appIconPNG []byte

// ---- App Identity ----

const appDisplayName = "pwaWarden"
const appMutexName   = "Global\\pwaWarden_SingleInstance"
const appIconIco     = "assets/pwaWarden.ico"

// ---- Advanced Config (advanced.ini) ----
//
// pwaWarden is designed to run as a single persistent instance and manage PWA
// process lifecycle automatically. These defaults are intentional and safe.
//
// To override, create "advanced.ini" in the same folder as pwaWarden.exe.
//
// WARNING: changing these defaults can result in stuck background processes,
// duplicate instances, and other unexpected behavior.
//
// Available keys (all default to false):
//   allowMultiplePwaWardenInstances = true  ; WARNING: instances fight over tray/process tracking
//   allowMultiplePWAInstances       = true  ; WARNING: causes duplicate/stuck background processes
//   allowSettingsWhileRunning       = true  ; WARNING: changes won't apply until PWA is restarted
//   skipExitConfirmation            = true  ; Skip the exit confirmation dialog (set automatically)

type AdvancedConfig struct {
	AllowMultiplePwaWardenInstances bool
	AllowMultiplePWAInstances       bool
	AllowSettingsWhileRunning       bool
	SkipExitConfirmation            bool
}

func loadAdvancedConfig(dir string) AdvancedConfig {
	cfg := AdvancedConfig{}
	f, err := os.Open(filepath.Join(dir, "advanced.ini"))
	if err != nil {
		return cfg
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == ';' || line[0] == '#' {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(strings.ToLower(parts[0]))
		val := strings.TrimSpace(strings.ToLower(parts[1]))
		on := val == "true" || val == "1" || val == "yes"
		switch key {
		case "allowmultiplepwawardeninstances":
			cfg.AllowMultiplePwaWardenInstances = on
		case "allowmultiplepwainstances":
			cfg.AllowMultiplePWAInstances = on
		case "allowsettingswhilerunning":
			cfg.AllowSettingsWhileRunning = on
		case "skipexitconfirmation":
			cfg.SkipExitConfirmation = on
		}
	}
	logInfo("advanced.ini loaded: %+v", cfg)
	return cfg
}

// setSkipExitConfirmation writes skipExitConfirmation=true to advanced.ini,
// creating the file if it doesn't exist.
func setSkipExitConfirmation(dir string) {
	path := filepath.Join(dir, "advanced.ini")

	// Read existing content if file exists
	existing := ""
	if data, err := os.ReadFile(path); err == nil {
		existing = string(data)
	}

	// Check if key already exists — update it
	lines := strings.Split(existing, "\n")
	found := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), "skipexitconfirmation") {
			lines[i] = "skipExitConfirmation = true"
			found = true
			break
		}
	}

	var content string
	if found {
		content = strings.Join(lines, "\n")
	} else {
		// Append the key with a comment header if file is new
		header := ""
		if existing == "" {
			header = "; pwaWarden - advanced.ini\n; Auto-generated. See documentation for all available keys.\n\n"
		}
		content = existing
		if len(content) > 0 && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += header + "skipExitConfirmation = true\n"
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		logError("Failed to write advanced.ini: %v", err)
	} else {
		logInfo("Wrote skipExitConfirmation=true to advanced.ini")
	}
}

// ---- Data Types ----

type PWAInfo struct {
	Name  string
	AppID string
}

type TrayMode string

const (
	TrayModeOff      TrayMode = "off"
	TrayModeMinimize TrayMode = "minimize"
	TrayModeClose    TrayMode = "close"
	TrayModeBoth     TrayMode = "both"
)

func (m TrayMode) Label() string {
	switch m {
	case TrayModeMinimize:
		return "Minimize to tray"
	case TrayModeClose:
		return "Close to tray"
	case TrayModeBoth:
		return "Minimize & close to tray"
	default:
		return "Off (normal)"
	}
}

var trayModeOptions = []string{
	TrayModeOff.Label(),
	TrayModeMinimize.Label(),
	TrayModeClose.Label(),
	TrayModeBoth.Label(),
}

func trayModeFromLabel(label string) TrayMode {
	switch label {
	case TrayModeMinimize.Label():
		return TrayModeMinimize
	case TrayModeClose.Label():
		return TrayModeClose
	case TrayModeBoth.Label():
		return TrayModeBoth
	default:
		return TrayModeOff
	}
}

type AppEntry struct {
	Name       string   `json:"name"`
	AppID      string   `json:"app_id"`
	IgnoreCert bool     `json:"ignore_cert"`
	Flags      []string `json:"flags"`
	TrayMode   TrayMode `json:"tray_mode,omitempty"`
}

type Config struct {
	Chrome string     `json:"chrome"`
	Apps   []AppEntry `json:"apps"`
}

// ---- Win32 Tray Hook ----

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procShowWindow               = user32.NewProc("ShowWindow")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procFindWindowEx             = user32.NewProc("FindWindowExW")
	procSetWinEventHook          = user32.NewProc("SetWinEventHook")
	procUnhookWinEvent           = user32.NewProc("UnhookWinEvent")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessage          = user32.NewProc("DispatchMessageW")
	procPostThreadMessage        = user32.NewProc("PostThreadMessageW")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procKeybdEvent               = user32.NewProc("keybd_event")
	procGetWindowPlacement       = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement       = user32.NewProc("SetWindowPlacement")
	procCreateWindowEx           = user32.NewProc("CreateWindowExW")
	procDefWindowProc            = user32.NewProc("DefWindowProcW")
	procRegisterClassEx          = user32.NewProc("RegisterClassExW")
	procLoadIcon                 = user32.NewProc("LoadIconW")
	procLoadImage                = user32.NewProc("LoadImageW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procPostMessage              = user32.NewProc("PostMessageW")
	procSendMessage              = user32.NewProc("SendMessageW")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procAppendMenu               = user32.NewProc("AppendMenuW")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procFlashWindowEx            = user32.NewProc("FlashWindowEx")

	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
	procExtractIconEx   = shell32.NewProc("ExtractIconExW")

	procGetCurrentThreadId       = kernel32.NewProc("GetCurrentThreadId")
	procGetModuleHandle          = kernel32.NewProc("GetModuleHandleW")
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32First           = kernel32.NewProc("Process32FirstW")
	procProcess32Next            = kernel32.NewProc("Process32NextW")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
	procCreateMutex              = kernel32.NewProc("CreateMutexW")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
	procReadProcessMemory        = kernel32.NewProc("ReadProcessMemory")
)

const (
	swHide    = 0
	swRestore = 9
	swShow    = 5
	swShowNA  = 8

	eventCaptureStart  = 0x0008
	eventMinimizeStart = 0x0016
	eventMinimizeEnd   = 0x0017
	eventObjectDestroy = 0x8001

	winEventOutOfContext   = 0x0000
	winEventSkipOwnProcess = 0x0002

	wmQuit    = 0x0012
	wmUser    = 0x0400
	wmTray    = wmUser + 1
	wmCommand = 0x0111

	nimAdd     = 0x00000000
	nimDelete  = 0x00000002
	nimSetTip  = 0x00000004
	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	tpmLeftButton  = 0x0000
	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020
	tpmRightAlign  = 0x0008
	tpmReturnCmd   = 0x0100
	tpmNoNotify    = 0x0080

	menuIDShow = 1001
	menuIDQuit = 1002

	mfString  = 0x00000000
	mfDefault = 0x00001000

	closeButtonWidth = 50
	titleBarHeight   = 40

	trayWindowClass = appDisplayName + "TrayMsg"
)

type winMSG struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

type winPOINT struct{ X, Y int32 }
type winRECT struct{ Left, Top, Right, Bottom int32 }

type windowPlacement struct {
	Length           uint32
	Flags            uint32
	ShowCmd          uint32
	MinPosition      winPOINT
	MaxPosition      winPOINT
	NormalPosition   winRECT
	Device           winRECT
}

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
	UTimeoutVersion  uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
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

var trayClassRegistered bool

func registerTrayClass() {
	if trayClassRegistered {
		return
	}
	className, _ := syscall.UTF16PtrFromString(trayWindowClass)
	hInst, _, _ := procGetModuleHandle.Call(0)
	wc := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		LpfnWndProc:   syscall.NewCallback(trayDefWndProc),
		HInstance:     hInst,
		LpszClassName: className,
	}
	ret, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	if ret == 0 {
		logError("RegisterClassEx failed: %v", err)
	} else {
		logInfo("Tray window class registered (atom=0x%X)", ret)
	}
	trayClassRegistered = true
}

func trayDefWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	if msg == wmTray {
		trayWindowsMu.Lock()
		s, ok := trayWindows[hwnd]
		trayWindowsMu.Unlock()
		if ok {
			handleTrayCallback(s, lParam)
		}
		return 0
	}
	ret, _, _ := procDefWindowProc.Call(hwnd, msg, wParam, lParam)
	return ret
}

func handleTrayCallback(s *traySession, lParam uintptr) {
	const (
		wmLButtonDblClk = 0x0203
		wmRButtonUp     = 0x0205
		wmLButtonUp     = 0x0202
	)
	switch lParam {
	case wmLButtonDblClk, wmLButtonUp:
		showTrayWindow(s)
	case wmRButtonUp:
		showTrayMenu(s)
	}
}

func showTrayWindow(s *traySession) {
	removeTrayIcon(s)
	logInfo("Restoring %s — saved ShowCmd=%d", s.appName, s.savedPlacement.ShowCmd)

	if s.savedPlacement.Length > 0 {
		if s.savedPlacement.ShowCmd == 2 {
			s.savedPlacement.ShowCmd = 1
		}
		s.savedPlacement.Length = uint32(unsafe.Sizeof(s.savedPlacement))
		procSetWindowPlacement.Call(s.hwnd, uintptr(unsafe.Pointer(&s.savedPlacement)))
		procShowWindow.Call(s.hwnd, swShow)
	} else {
		procShowWindow.Call(s.hwnd, swRestore)
	}

	var curr windowPlacement
	curr.Length = uint32(unsafe.Sizeof(curr))
	procGetWindowPlacement.Call(s.hwnd, uintptr(unsafe.Pointer(&curr)))
	if curr.ShowCmd == 2 {
		procShowWindow.Call(s.hwnd, swRestore)
		time.Sleep(80 * time.Millisecond)
	}
	procSetForegroundWindow.Call(s.hwnd)
	time.Sleep(60 * time.Millisecond)
	procSetForegroundWindow.Call(s.hwnd)

	// Flash taskbar
	type FLASHWINFO struct {
		cbSize    uint32
		hwnd      uintptr
		dwFlags   uint32
		uCount    uint32
		dwTimeout uint32
	}
	const (
		FLASHW_ALL       = 0x00000003
		FLASHW_TIMERNOFG = 0x0000000C
	)
	var f FLASHWINFO
	f.cbSize = uint32(unsafe.Sizeof(f))
	f.hwnd = s.hwnd
	f.dwFlags = FLASHW_ALL | FLASHW_TIMERNOFG
	f.uCount = 4
	f.dwTimeout = 0
	procFlashWindowEx.Call(uintptr(unsafe.Pointer(&f)))

	s.isHidden = false
	logInfo("Restore complete for %s (HWND 0x%X)", s.appName, s.hwnd)
}

func showTrayMenu(s *traySession) {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	showLabel, _ := syscall.UTF16PtrFromString("Show " + s.appName)
	quitLabel, _ := syscall.UTF16PtrFromString("Quit " + s.appName)
	procAppendMenu.Call(hMenu, mfString|mfDefault, menuIDShow, uintptr(unsafe.Pointer(showLabel)))
	procAppendMenu.Call(hMenu, mfString, menuIDQuit, uintptr(unsafe.Pointer(quitLabel)))

	var pt winPOINT
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(s.msgHWND)

	ret, _, _ := procTrackPopupMenu.Call(
		hMenu,
		tpmRightButton|tpmBottomAlign|tpmRightAlign|tpmReturnCmd|tpmNoNotify,
		uintptr(pt.X), uintptr(pt.Y),
		0, s.msgHWND, 0,
	)

	switch ret {
	case menuIDShow:
		showTrayWindow(s)
	case menuIDQuit:
		logInfo("Quit requested for %s via tray menu", s.appName)
		removeTrayIcon(s)
		killChromeProcessesWithAppID(s.appID, s.appName)
	}
}

func addTrayIcon(s *traySession) {
	if s.msgHWND == 0 {
		return
	}

	var hIconLarge uintptr

	icoPath := filepath.Join(
		baseDir,
		"data", "Default", "Web Applications",
		"_crx_"+s.appID,
		s.appName+".ico",
	)
	if _, err := os.Stat(icoPath); err == nil {
		icoPtr, _ := syscall.UTF16PtrFromString(icoPath)
		hIconLarge, _, _ = procLoadImage.Call(
			0,
			uintptr(unsafe.Pointer(icoPtr)),
			1,
			0, 0,
			0x0010|0x0040,
		)
		if hIconLarge != 0 {
			logInfo("Loaded PWA icon: %s", icoPath)
		}
	}

	if hIconLarge == 0 {
		exePath, _ := os.Executable()
		if exePath != "" {
			exePtr, _ := syscall.UTF16PtrFromString(exePath)
			procExtractIconEx.Call(uintptr(unsafe.Pointer(exePtr)), 0,
				uintptr(unsafe.Pointer(&hIconLarge)), 0, 1)
		}
	}
	if hIconLarge == 0 {
		icoFallback := filepath.Join(baseDir, appIconIco)
		if _, err := os.Stat(icoFallback); err == nil {
			icoPtr, _ := syscall.UTF16PtrFromString(icoFallback)
			hIconLarge, _, _ = procLoadImage.Call(0, uintptr(unsafe.Pointer(icoPtr)),
				1, 0, 0, 0x0010|0x0040)
		}
	}
	if hIconLarge == 0 {
		hIconLarge, _, _ = procLoadIcon.Call(0, 0x7F00)
	}
	if hIconLarge == 0 {
		hInst, _, _ := procGetModuleHandle.Call(0)
		hIconLarge, _, _ = procLoadIcon.Call(hInst, 0x7F00)
	}

	tipStr := s.appName + " (click to restore)"
	nid := notifyIconData{
		HWnd:             s.msgHWND,
		UID:              s.trayUID,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: wmTray,
		HIcon:            hIconLarge,
	}
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	tipSlice := syscall.StringToUTF16(tipStr)
	for i, c := range tipSlice {
		if i >= 128 {
			break
		}
		nid.SzTip[i] = c
	}

	ret, _, err := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	if ret == 0 {
		logError("Shell_NotifyIcon add failed for %s: %v", s.appName, err)
	} else {
		logInfo("Tray icon added for %s", s.appName)
	}
}

func removeTrayIcon(s *traySession) {
	if s.msgHWND == 0 {
		return
	}
	nid := notifyIconData{
		HWnd: s.msgHWND,
		UID:  s.trayUID,
	}
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	logInfo("Tray icon removed for %s", s.appName)
}

var (
	trayWindows   = map[uintptr]*traySession{}
	trayWindowsMu sync.Mutex
	trayUIDSeq    uint32 = 1
)

func createTrayMsgWindow(s *traySession) uintptr {
	registerTrayClass()
	className, _ := syscall.UTF16PtrFromString(trayWindowClass)
	hInst, _, _ := procGetModuleHandle.Call(0)
	hwnd, _, err := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(className)),
		0,
		0, 0, 0, 0,
		0xFFFFFFFFFFFFFFFD,
		0, hInst, 0,
	)
	if hwnd == 0 {
		logError("CreateWindowEx failed for %s: %v", s.appName, err)
		return 0
	}
	trayWindowsMu.Lock()
	trayWindows[hwnd] = s
	trayWindowsMu.Unlock()
	return hwnd
}

type traySession struct {
	hwnd           uintptr
	pid            uint32
	mode           TrayMode
	appName        string
	appID          string
	hookHandle     uintptr
	hookHandle2    uintptr
	threadID       uint32
	isHidden       bool
	msgHWND        uintptr
	trayUID        uint32
	savedPlacement windowPlacement
}

func getProcessTree(rootPID uint32) map[uint32]bool {
	snap, _, _ := procCreateToolhelp32Snapshot.Call(0x00000002, 0)
	if snap == 0 || snap == ^uintptr(0) {
		return map[uint32]bool{rootPID: true}
	}
	defer procCloseHandle.Call(snap)

	type processEntry struct {
		dwSize              uint32
		cntUsage            uint32
		th32ProcessID       uint32
		th32DefaultHeapID   uintptr
		th32ModuleID        uint32
		cntThreads          uint32
		th32ParentProcessID uint32
		pcPriClassBase      int32
		dwFlags             uint32
		szExeFile           [260]uint16
	}

	children := map[uint32][]uint32{}
	var entry processEntry
	entry.dwSize = uint32(unsafe.Sizeof(entry))
	ret, _, _ := procProcess32First.Call(snap, uintptr(unsafe.Pointer(&entry)))
	for ret != 0 {
		children[entry.th32ParentProcessID] = append(children[entry.th32ParentProcessID], entry.th32ProcessID)
		entry.dwSize = uint32(unsafe.Sizeof(entry))
		ret, _, _ = procProcess32Next.Call(snap, uintptr(unsafe.Pointer(&entry)))
	}

	result := map[uint32]bool{}
	queue := []uint32{rootPID}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		result[pid] = true
		queue = append(queue, children[pid]...)
	}
	return result
}

func findWindowByPID(pid uint32) uintptr {
	return findWindowForPWA(pid, "")
}

func findWindowForPWA(pid uint32, appName string) uintptr {
	var found uintptr
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pids := getProcessTree(pid)

		// Collect HWNDs already tracked so we don't steal another PWA's window
		trayWindowsMu.Lock()
		trackedHWNDs := make(map[uintptr]bool, len(trayWindows))
		for _, s := range trayWindows {
			if s.hwnd != 0 {
				trackedHWNDs[s.hwnd] = true
			}
		}
		trayWindowsMu.Unlock()

		titleBuf := make([]uint16, 512)
		cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
			if found != 0 {
				return 0
			}
			if trackedHWNDs[hwnd] {
				return 1
			}
			vis, _, _ := procIsWindowVisible.Call(hwnd)
			if vis == 0 {
				return 1
			}
			var r winRECT
			procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
			if (r.Right-r.Left) <= 200 || (r.Bottom-r.Top) <= 100 {
				return 1
			}
			// If we have an app name, prefer title match — more reliable than
			// process tree when Chrome shares renderer processes across PWAs
			if appName != "" {
				procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&titleBuf[0])), 512)
				title := syscall.UTF16ToString(titleBuf)
				if strings.Contains(title, appName) {
					found = hwnd
					return 0
				}
			}
			// Fallback: process tree match
			var wpid uint32
			procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&wpid)))
			if pids[wpid] {
				found = hwnd
			}
			return 1
		})
		procEnumWindows.Call(cb, 0)
		if found != 0 {
			return found
		}
		time.Sleep(500 * time.Millisecond)
	}
	return 0
}

func isCursorOverCloseButton(hwnd uintptr) bool {
	var cursor winPOINT
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
	var rect winRECT
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect)))
	inX := cursor.X >= rect.Right-closeButtonWidth && cursor.X <= rect.Right
	inY := cursor.Y >= rect.Top && cursor.Y <= rect.Top+titleBarHeight
	return inX && inY
}

func startTrayHook(s *traySession) {
	go func() {
		logInfo("Tray hook starting for %s (PID %d mode %s)", s.appName, s.pid, s.mode)

		trayWindowsMu.Lock()
		s.trayUID = trayUIDSeq
		trayUIDSeq++
		trayWindowsMu.Unlock()

		hwnd := findWindowForPWA(s.pid, s.appName)
		if hwnd == 0 {
			logError("Could not find window for %s (PID %d)", s.appName, s.pid)
			return
		}
		s.hwnd = hwnd
		logInfo("Found HWND 0x%X for %s", hwnd, s.appName)

		s.msgHWND = createTrayMsgWindow(s)
		if s.msgHWND == 0 {
			logError("Could not create message window for %s", s.appName)
			return
		}

		hideWindow := func() {
			if s.isHidden {
				logInfo("Already hidden — skipping for %s", s.appName)
				return
			}
			var placement windowPlacement
			placement.Length = uint32(unsafe.Sizeof(placement))
			procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&placement)))
			if placement.ShowCmd == 2 {
				placement.ShowCmd = 1
			}
			s.savedPlacement = placement
			logInfo("Saved ShowCmd for %s: %d", s.appName, s.savedPlacement.ShowCmd)
			procShowWindow.Call(hwnd, swHide)
			s.isHidden = true
			addTrayIcon(s)
			logInfo("Hidden %s, tray icon added", s.appName)
		}

		cb := syscall.NewCallback(func(hHook, event uintptr, hwnd uintptr, idObj, idChild int32, thread, ms uint32) uintptr {
			if hwnd != s.hwnd {
				return 0
			}
			switch event {
			case eventCaptureStart:
				if (s.mode == TrayModeClose || s.mode == TrayModeBoth) && isCursorOverCloseButton(hwnd) {
					logInfo("Close intercepted for %s", s.appName)
					hideWindow()
				}
			case eventMinimizeStart:
				if s.mode == TrayModeMinimize || s.mode == TrayModeBoth {
					logInfo("Minimize intercepted for %s", s.appName)
					hideWindow()
				}
			case eventMinimizeEnd:
				logInfo("Minimize end for %s (isHidden=%v)", s.appName, s.isHidden)
				if s.isHidden {
					removeTrayIcon(s)
				}
				s.isHidden = false
			case eventObjectDestroy:
				logInfo("Window destroyed for %s", s.appName)
				removeTrayIcon(s)
				procUnhookWinEvent.Call(s.hookHandle)
				if s.hookHandle2 != 0 {
					procUnhookWinEvent.Call(s.hookHandle2)
				}
				trayWindowsMu.Lock()
				delete(trayWindows, s.msgHWND)
				trayWindowsMu.Unlock()
				procDestroyWindow.Call(s.msgHWND)
				procPostThreadMessage.Call(uintptr(s.threadID), wmQuit, 0, 0)
			}
			return 0
		})

		hookHandle, _, _ := procSetWinEventHook.Call(
			eventCaptureStart, eventMinimizeEnd, 0, cb,
			0, 0, winEventOutOfContext,
		)
		if hookHandle == 0 {
			logError("SetWinEventHook (capture/minimize) failed for %s", s.appName)
			return
		}
		s.hookHandle = hookHandle

		hookHandle2, _, _ := procSetWinEventHook.Call(
			eventObjectDestroy, eventObjectDestroy, 0, cb,
			0, 0, winEventOutOfContext,
		)
		if hookHandle2 == 0 {
			logError("SetWinEventHook (destroy) failed for %s", s.appName)
			procUnhookWinEvent.Call(s.hookHandle)
			return
		}
		s.hookHandle2 = hookHandle2

		tid, _, _ := procGetCurrentThreadId.Call()
		s.threadID = uint32(tid)
		logInfo("Tray hooks attached for %s (h1=0x%X h2=0x%X)", s.appName, s.hookHandle, s.hookHandle2)

		var msg winMSG
		for {
			ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if ret == 0 || ret == ^uintptr(0) {
				break
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
		}
		logInfo("Tray hook exited for %s", s.appName)
	}()
}

// ---- Globals & Helpers ----

var logger *log.Logger
var baseDir string

func initLogger() {
	logDir := filepath.Join(baseDir, "log")
	os.MkdirAll(logDir, 0755)
	logFile := filepath.Join(logDir, fmt.Sprintf("launcher-%s.log", time.Now().Format("2006-01-02")))
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		logger = log.New(os.Stderr, "", log.LstdFlags)
		return
	}
	logger = log.New(f, "", log.LstdFlags)
}

func logInfo(format string, args ...interface{}) {
	if logger != nil {
		logger.Printf("[INFO] "+format, args...)
	}
}

func logError(format string, args ...interface{}) {
	if logger != nil {
		logger.Printf("[ERROR] "+format, args...)
	}
}

func centerWindow(w fyne.Window) {
	w.CenterOnScreen()
}

func loadConfig() *Config {
	config := &Config{
		Chrome: "app/chrome.exe",
		Apps:   []AppEntry{},
	}
	data, err := os.ReadFile(filepath.Join(baseDir, "config.json"))
	if err != nil {
		return config
	}
	json.Unmarshal(data, config)
	return config
}

func saveConfig(config *Config) error {
	data, err := json.MarshalIndent(config, "", "\t")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(baseDir, "config.json"), data, 0644)
}

// ---- PWA Scanner & Launch ----

func scanPWAs() ([]PWAInfo, error) {
	webAppsDir := filepath.Join(baseDir, "data", "Default", "Web Applications")
	entries, err := os.ReadDir(webAppsDir)
	if err != nil {
		return nil, fmt.Errorf("Web Applications directory not found")
	}
	var pwas []PWAInfo
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "_crx_") {
			continue
		}
		appID := strings.TrimPrefix(entry.Name(), "_crx_")
		files, err := os.ReadDir(filepath.Join(webAppsDir, entry.Name()))
		if err != nil {
			continue
		}
		var appName string
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".lnk") {
				appName = strings.TrimSuffix(f.Name(), ".lnk")
				break
			}
		}
		if appName == "" {
			continue
		}
		pwas = append(pwas, PWAInfo{Name: appName, AppID: appID})
	}
	return pwas, nil
}

func getChromePath(config *Config) string {
	return filepath.Join(baseDir, config.Chrome)
}

func launchSetup(config *Config) error {
	cmd := exec.Command(getChromePath(config),
		"--user-data-dir="+filepath.Join(baseDir, "data"),
		"--profile-directory=Default",
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch Chrome: %w", err)
	}
	logInfo("Chrome setup launched (PID %d)", cmd.Process.Pid)
	return nil
}

// ---- Process Detection ----

var (
	ntdll                  = syscall.NewLazyDLL("ntdll.dll")
	procNtQueryInfoProcess = ntdll.NewProc("NtQueryInformationProcess")
)

const (
	processBasicInformation = 0
	processCmdLineInfo      = 60
	processQueryLimitedInfo = 0x1000
	processVMRead           = 0x0010
)

type processBasicInfo struct {
	Reserved1       uintptr
	PebBaseAddress  uintptr
	Reserved2       [2]uintptr
	UniqueProcessID uintptr
	Reserved3       uintptr
}

type unicodeString struct {
	Length        uint16
	MaximumLength uint16
	_             [4]byte
	Buffer        uintptr
}

func getChromeCommandLine(pid uint32) string {
	hProc, _, _ := procOpenProcess.Call(processQueryLimitedInfo|processVMRead, 0, uintptr(pid))
	if hProc == 0 {
		return ""
	}
	defer procCloseHandle.Call(hProc)

	var info struct {
		Length        uint16
		MaximumLength uint16
		_             [4]byte
		Buffer        uintptr
	}
	var retLen uint32
	status, _, _ := procNtQueryInfoProcess.Call(
		hProc, processCmdLineInfo,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info),
		uintptr(unsafe.Pointer(&retLen)),
	)
	if status == 0 && info.Buffer != 0 && info.Length > 0 {
		buf := make([]uint16, info.Length/2)
		var read uintptr
		r, _, _ := procReadProcessMemory.Call(hProc, info.Buffer,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(info.Length),
			uintptr(unsafe.Pointer(&read)))
		if r != 0 {
			return syscall.UTF16ToString(buf)
		}
	}

	var pbi processBasicInfo
	status, _, _ = procNtQueryInfoProcess.Call(
		hProc, processBasicInformation,
		uintptr(unsafe.Pointer(&pbi)), unsafe.Sizeof(pbi),
		uintptr(unsafe.Pointer(&retLen)),
	)
	if status != 0 || pbi.PebBaseAddress == 0 {
		return ""
	}
	var procParamsPtr uintptr
	var read uintptr
	procReadProcessMemory.Call(hProc, pbi.PebBaseAddress+0x20,
		uintptr(unsafe.Pointer(&procParamsPtr)), unsafe.Sizeof(procParamsPtr),
		uintptr(unsafe.Pointer(&read)))
	if procParamsPtr == 0 {
		return ""
	}
	var cmdLine unicodeString
	procReadProcessMemory.Call(hProc, procParamsPtr+0x70,
		uintptr(unsafe.Pointer(&cmdLine)), unsafe.Sizeof(cmdLine),
		uintptr(unsafe.Pointer(&read)))
	if cmdLine.Buffer == 0 || cmdLine.Length == 0 {
		return ""
	}
	buf := make([]uint16, cmdLine.Length/2)
	r, _, _ := procReadProcessMemory.Call(hProc, cmdLine.Buffer,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(cmdLine.Length),
		uintptr(unsafe.Pointer(&read)))
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

func findChromeProcessesWithAppID(appID string) []uint32 {
	snap, _, _ := procCreateToolhelp32Snapshot.Call(0x00000002, 0)
	if snap == 0 || snap == ^uintptr(0) {
		return nil
	}
	defer procCloseHandle.Call(snap)

	type pe struct {
		dwSize              uint32
		cntUsage            uint32
		th32ProcessID       uint32
		th32DefaultHeapID   uintptr
		th32ModuleID        uint32
		cntThreads          uint32
		th32ParentProcessID uint32
		pcPriClassBase      int32
		dwFlags             uint32
		szExeFile           [260]uint16
	}

	needle := "--app-id=" + appID
	var matches []uint32
	var entry pe
	entry.dwSize = uint32(unsafe.Sizeof(entry))
	ret, _, _ := procProcess32First.Call(snap, uintptr(unsafe.Pointer(&entry)))
	for ret != 0 {
		if strings.EqualFold(syscall.UTF16ToString(entry.szExeFile[:]), "chrome.exe") {
			if strings.Contains(getChromeCommandLine(entry.th32ProcessID), needle) {
				matches = append(matches, entry.th32ProcessID)
			}
		}
		entry.dwSize = uint32(unsafe.Sizeof(entry))
		ret, _, _ = procProcess32Next.Call(snap, uintptr(unsafe.Pointer(&entry)))
	}
	return matches
}

func killChromeProcessesWithAppID(appID, appName string) {
	for _, pid := range findChromeProcessesWithAppID(appID) {
		logInfo("Killing chrome PID %d for %s", pid, appName)
		cmd := exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", pid))
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := cmd.Run(); err != nil {
			logError("taskkill PID %d: %v", pid, err)
		}
	}
	time.Sleep(400 * time.Millisecond)
}

func isPWARunning(appID string) bool {
	return len(findChromeProcessesWithAppID(appID)) > 0
}

func findSessionByAppID(appID string) *traySession {
	trayWindowsMu.Lock()
	defer trayWindowsMu.Unlock()
	for _, sess := range trayWindows {
		if sess.appID == appID {
			return sess
		}
	}
	return nil
}

func cleanupAllPWAs(config *Config) {
	for _, a := range config.Apps {
		if isPWARunning(a.AppID) {
			logInfo("Shutdown cleanup: killing %s", a.Name)
			killChromeProcessesWithAppID(a.AppID, a.Name)
		}
	}
}

// ---- pwaWarden Own Tray Icon ----
//
// When the pwaWarden window is minimized, it hides to its own tray icon.
// Double-click or left-click restores it. Right-click shows a menu with
// Show and Exit options.

const (
	menuIDWardenShow = 2001
	menuIDWardenExit = 2002
)

type wardenTrayState struct {
	hwnd           uintptr
	msgHWND        uintptr
	threadID       uint32
	trayUID        uint32
	savedPlacement windowPlacement
}

var wardenTray *wardenTrayState

func addWardenTrayIcon(hwnd uintptr) {
	if wardenTray == nil {
		return
	}

	icoPath := filepath.Join(baseDir, appIconIco)
	var hIcon uintptr
	if _, err := os.Stat(icoPath); err == nil {
		icoPtr, _ := syscall.UTF16PtrFromString(icoPath)
		hIcon, _, _ = procLoadImage.Call(
			0, uintptr(unsafe.Pointer(icoPtr)),
			1, 0, 0, 0x0010|0x0040,
		)
	}
	if hIcon == 0 {
		hIcon, _, _ = procLoadIcon.Call(0, 0x7F00)
	}

	tip, _ := syscall.UTF16FromString(appDisplayName)
	nid := notifyIconData{
		CbSize:           uint32(unsafe.Sizeof(notifyIconData{})),
		HWnd:             wardenTray.msgHWND,
		UID:              wardenTray.trayUID,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: wmTray,
		HIcon:            hIcon,
	}
	copy(nid.SzTip[:], tip)
	procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	logInfo("pwaWarden tray icon added")
}

const wmWardenRemoveTray = wmUser + 2

func removeWardenTrayIcon() {
	if wardenTray == nil {
		return
	}
	// Post to the warden tray thread so it runs on the correct message loop
	if wardenTray.threadID != 0 {
		procPostThreadMessage.Call(uintptr(wardenTray.threadID), wmWardenRemoveTray, 0, 0)
	} else {
		nid := notifyIconData{
			CbSize: uint32(unsafe.Sizeof(notifyIconData{})),
			HWnd:   wardenTray.msgHWND,
			UID:    wardenTray.trayUID,
		}
		procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	}
}

var wardenOnExit func()

func showWardenTrayMenu(mainHWND uintptr) {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	showLabel, _ := syscall.UTF16PtrFromString("Show " + appDisplayName)
	exitLabel, _ := syscall.UTF16PtrFromString("Exit " + appDisplayName)
	procAppendMenu.Call(hMenu, mfString|mfDefault, menuIDWardenShow, uintptr(unsafe.Pointer(showLabel)))
	procAppendMenu.Call(hMenu, mfString, menuIDWardenExit, uintptr(unsafe.Pointer(exitLabel)))

	var pt winPOINT
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(wardenTray.msgHWND)

	ret, _, _ := procTrackPopupMenu.Call(
		hMenu,
		tpmRightButton|tpmBottomAlign|tpmRightAlign|tpmReturnCmd|tpmNoNotify,
		uintptr(pt.X), uintptr(pt.Y),
		0, wardenTray.msgHWND, 0,
	)

	switch ret {
	case menuIDWardenShow:
		restoreWardenWindow(mainHWND)
	case menuIDWardenExit:
		if wardenOnExit != nil {
			restoreWardenWindow(mainHWND)
			time.Sleep(150 * time.Millisecond)
			wardenOnExit()
		}
	}
}

func restoreWardenWindow(hwnd uintptr) {
	logInfo("Restoring pwaWarden window (HWND=0x%X, savedShowCmd=%d)", hwnd, func() uint32 {
		if wardenTray != nil {
			return wardenTray.savedPlacement.ShowCmd
		}
		return 0
	}())

	if wardenTray != nil && wardenTray.savedPlacement.Length > 0 {
		if wardenTray.savedPlacement.ShowCmd == 2 {
			wardenTray.savedPlacement.ShowCmd = 1
		}
		wardenTray.savedPlacement.Length = uint32(unsafe.Sizeof(wardenTray.savedPlacement))
		procSetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&wardenTray.savedPlacement)))
		procShowWindow.Call(hwnd, swShow)
	} else {
		procShowWindow.Call(hwnd, swRestore)
	}

	// Post-restore check — if still minimized, force it
	var curr windowPlacement
	curr.Length = uint32(unsafe.Sizeof(curr))
	procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&curr)))
	if curr.ShowCmd == 2 {
		procShowWindow.Call(hwnd, swRestore)
		time.Sleep(80 * time.Millisecond)
	}
	procSetForegroundWindow.Call(hwnd)
	time.Sleep(60 * time.Millisecond)
	procSetForegroundWindow.Call(hwnd)
}

// startWardenTray starts the message loop for pwaWarden's own tray icon.
// It runs in its own goroutine. mainHWND is the Fyne window handle,
// used to restore the window on tray click.
func startWardenTray(mainHWND uintptr, onExit func()) {
	wardenOnExit = onExit

	const wardenTrayClass = appDisplayName + "WardenTray"
	clsName, _ := syscall.UTF16PtrFromString(wardenTrayClass)
	hInst, _, _ := procGetModuleHandle.Call(0)

	// Handle tray messages directly in the wndproc
	wndProc := syscall.NewCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
		if msg == wmTray {
			switch lp {
			case 0x0201, 0x0203: // WM_LBUTTONUP, WM_LBUTTONDBLCLK
				logInfo("pwaWarden tray clicked — restoring")
				restoreWardenWindow(mainHWND)
			case 0x0205: // WM_RBUTTONUP
				showWardenTrayMenu(mainHWND)
			}
			return 0
		}
		if msg == wmWardenRemoveTray {
			nid := notifyIconData{
				CbSize: uint32(unsafe.Sizeof(notifyIconData{})),
				HWnd:   hwnd,
				UID:    wardenTray.trayUID,
			}
			procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
			return 0
		}
		ret, _, _ := procDefWindowProc.Call(hwnd, msg, wp, lp)
		return ret
	})

	wc := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		LpfnWndProc:   wndProc,
		HInstance:     hInst,
		LpszClassName: clsName,
	}
	ret, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	if ret == 0 {
		logError("startWardenTray: RegisterClassEx failed: %v", err)
		return
	}

	msgHWND, _, err := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(clsName)),
		uintptr(unsafe.Pointer(clsName)),
		0,
		0, 0, 0, 0,
		0xFFFFFFFFFFFFFFFD, // HWND_MESSAGE
		0, hInst, 0,
	)
	if msgHWND == 0 {
		logError("startWardenTray: CreateWindowEx failed: %v", err)
		return
	}

	tid, _, _ := procGetCurrentThreadId.Call()
	wardenTray = &wardenTrayState{
		hwnd:     mainHWND,
		msgHWND:  msgHWND,
		threadID: uint32(tid),
		trayUID:  9999,
	}
	logInfo("pwaWarden tray message window created (HWND=0x%X)", msgHWND)

	// Message loop — wndproc handles wmTray directly
	var msg winMSG
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if ret == 0 || ret == ^uintptr(0) {
			break
		}
		// Thread messages (hwnd==0) aren't dispatched to wndproc
		if msg.HWnd == 0 && msg.Message == wmWardenRemoveTray {
			if wardenTray != nil {
				nid := notifyIconData{
					CbSize: uint32(unsafe.Sizeof(notifyIconData{})),
					HWnd:   wardenTray.msgHWND,
					UID:    wardenTray.trayUID,
				}
				procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
			}
			continue
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func launchPWA(config *Config, a AppEntry) error {
	args := []string{
		"--user-data-dir=" + filepath.Join(baseDir, "data"),
		"--profile-directory=Default",
		"--app-id=" + a.AppID,
	}
	if a.IgnoreCert {
		args = append(args, "--ignore-certificate-errors")
	}
	args = append(args, a.Flags...)
	cmd := exec.Command(getChromePath(config), args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch %s: %w", a.Name, err)
	}
	logInfo("Launched %s (PID %d)", a.Name, cmd.Process.Pid)

	if a.TrayMode != TrayModeOff && a.TrayMode != "" {
		s := &traySession{
			pid:     uint32(cmd.Process.Pid),
			mode:    a.TrayMode,
			appName: a.Name,
			appID:   a.AppID,
		}
		startTrayHook(s)
	}
	return nil
}

// ---- Shortcut, Download, Extract, Dialogs, UI, Main ----

func createDesktopShortcut(a AppEntry) error {
	desktopPath := filepath.Join(os.Getenv("USERPROFILE"), "Desktop")
	shortcutPath := filepath.Join(desktopPath, a.Name+".lnk")

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine launcher path: %w", err)
	}

	ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED|ole.COINIT_SPEED_OVER_MEMORY)
	defer ole.CoUninitialize()

	oleShellObject, err := oleutil.CreateObject("WScript.Shell")
	if err != nil {
		return fmt.Errorf("could not create WScript.Shell: %w", err)
	}
	defer oleShellObject.Release()

	wshell, err := oleShellObject.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return fmt.Errorf("could not get IDispatch: %w", err)
	}
	defer wshell.Release()

	cs, err := oleutil.CallMethod(wshell, "CreateShortcut", shortcutPath)
	if err != nil {
		return fmt.Errorf("could not create shortcut object: %w", err)
	}
	dispatch := cs.ToIDispatch()
	defer dispatch.Release()

	oleutil.PutProperty(dispatch, "TargetPath", exePath)
	oleutil.PutProperty(dispatch, "Arguments", `"`+a.Name+`"`)
	oleutil.PutProperty(dispatch, "WorkingDirectory", baseDir)
	oleutil.PutProperty(dispatch, "Description", "Launch "+a.Name+" PWA")
	oleutil.CallMethod(dispatch, "Save")

	logInfo("Created desktop shortcut for %s at %s", a.Name, shortcutPath)
	return nil
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func fetchLatestChromeRelease() (downloadURL, filename string, err error) {
	const apiURL = "https://api.github.com/repos/portapps/ungoogled-chromium-portable/releases/latest"
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return "", "", fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", "", fmt.Errorf("failed to parse GitHub API response: %w", err)
	}

	for _, asset := range release.Assets {
		name := strings.ToLower(asset.Name)
		if strings.Contains(name, "win64") && strings.HasSuffix(name, ".7z") {
			return asset.BrowserDownloadURL, asset.Name, nil
		}
	}
	return "", "", fmt.Errorf("no win64 .7z asset found")
}

func downloadFile(url, destPath string, onProgress func(downloaded, total int64)) error {
	client := &http.Client{Timeout: 0}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("download request failed: %w", err)
	}
	defer resp.Body.Close()

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("could not create file %s: %w", destPath, err)
	}
	defer f.Close()

	total := resp.ContentLength
	var downloaded int64
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := f.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write failed: %w", writeErr)
			}
			downloaded += int64(n)
			if onProgress != nil {
				onProgress(downloaded, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("download read error: %w", readErr)
		}
	}
	return nil
}

func extract7z(archivePath, destDir string, onProgress func(status string)) error {
	sevenZip := filepath.Join(baseDir, "7za.exe")
	if _, err := os.Stat(sevenZip); err != nil {
		return fmt.Errorf("7za.exe not found at %s", sevenZip)
	}

	cmd := exec.Command(sevenZip, "x", archivePath, "-o"+destDir, "-y", "-bsp1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	stdout, err := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err != nil {
		return fmt.Errorf("could not create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start 7za.exe: %w", err)
	}

	buf := make([]byte, 256)
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 && onProgress != nil {
			line := strings.TrimSpace(string(buf[:n]))
			if line != "" {
				onProgress(line)
			}
		}
		if readErr != nil {
			break
		}
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("extraction failed: %w", err)
	}

	logInfo("Extraction complete")

	os.Remove(archivePath)
	return nil
}

func trayModeDescription(mode TrayMode) string {
	switch mode {
	case TrayModeMinimize:
		return "Minimize → hides window to system tray (no taskbar). Close (X) exits normally."
	case TrayModeClose:
		return "Close (X) → hides app to system tray. Minimize → taskbar normally."
	case TrayModeBoth:
		return "Minimize & close → hide window to system tray."
	default:
		return "Normal: minimize to taskbar, close exits app."
	}
}

func showEditDialog(parentWin fyne.Window, config *Config, index int, appList *widget.List, advCfg AdvancedConfig) {
	a := config.Apps[index]

	nameEntry := widget.NewEntry()
	nameEntry.SetText(a.Name)
	nameEntry.SetPlaceHolder("Display name")

	appIDLabel := widget.NewLabel(a.AppID)
	appIDLabel.Wrapping = fyne.TextWrapWord

	ignoreCertCheck := widget.NewCheck("Ignore certificate errors (use for self-signed HTTPS)", nil)
	ignoreCertCheck.SetChecked(a.IgnoreCert)

	flagsEntry := widget.NewEntry()
	flagsEntry.SetText(strings.Join(a.Flags, " "))
	flagsEntry.SetPlaceHolder("e.g. --disable-web-security  (space separated)")

	currentMode := a.TrayMode
	if currentMode == "" {
		currentMode = TrayModeOff
	}

	trayDesc := widget.NewLabel(trayModeDescription(currentMode))
	trayDesc.Wrapping = fyne.TextWrapWord

	trayRadio := widget.NewRadioGroup(trayModeOptions, func(selected string) {
		trayDesc.SetText(trayModeDescription(trayModeFromLabel(selected)))
	})
	trayRadio.SetSelected(currentMode.Label())
	trayRadio.Required = true

	sectionLabel := func(text string) *widget.RichText {
		return widget.NewRichText(&widget.TextSegment{
			Text: text,
			Style: widget.RichTextStyle{
				SizeName:  theme.SizeNameSubHeadingText,
				TextStyle: fyne.TextStyle{Bold: true},
			},
		})
	}
	dimLabel := func(text string) *widget.Label {
		l := widget.NewLabel(text)
		l.Wrapping = fyne.TextWrapWord
		return l
	}

	content := container.NewVBox(
		sectionLabel("General"),
		widget.NewSeparator(),
		container.NewGridWithColumns(2,
			widget.NewLabel("Name"),
			nameEntry,
		),
		container.NewGridWithColumns(2,
			widget.NewLabel("App ID"),
			appIDLabel,
		),
		widget.NewSeparator(),

		sectionLabel("Options"),
		widget.NewSeparator(),
		ignoreCertCheck,
		widget.NewSeparator(),

		sectionLabel("Tray Behaviour"),
		widget.NewSeparator(),
		dimLabel("Controls what happens when you minimise or close this app's window."),
		container.NewPadded(trayRadio),
		container.NewPadded(trayDesc),
		widget.NewSeparator(),

		sectionLabel("Advanced"),
		widget.NewSeparator(),
		container.NewGridWithColumns(2,
			widget.NewLabel("Extra flags"),
			flagsEntry,
		),
		widget.NewLabel(""),
	)

	scroll := container.NewVScroll(content)
	scroll.SetMinSize(fyne.NewSize(520, 480))

	saveBtn := widget.NewButtonWithIcon("Save", theme.DocumentSaveIcon(), nil)
	saveBtn.Importance = widget.HighImportance
	cancelBtn := widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), nil)
	buttons := container.NewGridWithColumns(2, saveBtn, cancelBtn)
	fullContent := container.NewBorder(nil, buttons, nil, nil, scroll)

	dlg := dialog.NewCustomWithoutButtons("Settings — "+a.Name, fullContent, parentWin)
	dlg.Resize(fyne.NewSize(560, 640))

	saveBtn.OnTapped = func() {
		name := strings.TrimSpace(nameEntry.Text)
		if name == "" {
			dialog.ShowError(fmt.Errorf("name cannot be empty"), parentWin)
			return
		}
		var flags []string
		if f := strings.TrimSpace(flagsEntry.Text); f != "" {
			flags = strings.Fields(f)
		}
		doSave := func() {
			config.Apps[index].Name = name
			config.Apps[index].IgnoreCert = ignoreCertCheck.Checked
			config.Apps[index].Flags = flags
			config.Apps[index].TrayMode = trayModeFromLabel(trayRadio.Selected)
			if err := saveConfig(config); err != nil {
				logError("Save failed: %v", err)
				dialog.ShowError(err, parentWin)
				return
			}
			appList.Refresh()
			logInfo("Settings saved for %s", name)
			dlg.Hide()
		}
		if !advCfg.AllowSettingsWhileRunning && isPWARunning(a.AppID) {
			label := widget.NewLabel(fmt.Sprintf(
				"'%s' is currently running.\n\nSettings cannot be applied to a live process.\n\nClose the app and save, or discard your changes?",
				a.Name,
			))
			label.Wrapping = fyne.TextWrapWord
			closeAndSaveBtn := widget.NewButtonWithIcon("Close App & Save", theme.DocumentSaveIcon(), nil)
			closeAndSaveBtn.Importance = widget.HighImportance
			discardBtn := widget.NewButtonWithIcon("Discard Changes", theme.CancelIcon(), nil)
			conflict := dialog.NewCustomWithoutButtons("App Is Running", container.NewBorder(
				nil, container.NewGridWithColumns(2, closeAndSaveBtn, discardBtn), nil, nil,
				container.NewPadded(label),
			), parentWin)
			conflict.Resize(fyne.NewSize(420, 200))
			closeAndSaveBtn.OnTapped = func() {
				conflict.Hide()
				killChromeProcessesWithAppID(a.AppID, a.Name)
				doSave()
			}
			discardBtn.OnTapped = func() { conflict.Hide(); dlg.Hide() }
			conflict.Show()
			return
		}
		doSave()
	}
	cancelBtn.OnTapped = func() { dlg.Hide() }
	dlg.Show()
}

// ---- Shortcut Dialog ----

func showShortcutDialog(parentWin fyne.Window, config *Config, index int) {
	a := config.Apps[index]
	exePath, _ := os.Executable()

	msg := fmt.Sprintf(
		"This will create a desktop shortcut to launch '%s' directly.\n\n"+
			"⚠️  The shortcut will be hardcoded to this location:\n%s\n\n"+
			"If you move the launcher folder the shortcut will stop working and will need to be recreated.",
		a.Name, exePath,
	)

	label := widget.NewLabel(msg)
	label.Wrapping = fyne.TextWrapWord

	createBtn := widget.NewButtonWithIcon("Create Shortcut", theme.FileIcon(), nil)
	createBtn.Importance = widget.HighImportance
	cancelBtn := widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), nil)

	buttons := container.NewGridWithColumns(2, createBtn, cancelBtn)
	content := container.NewBorder(nil, buttons, nil, nil, container.NewPadded(label))

	dlg := dialog.NewCustomWithoutButtons("Create Desktop Shortcut", content, parentWin)
	dlg.Resize(fyne.NewSize(460, 240))

	createBtn.OnTapped = func() {
		if err := createDesktopShortcut(a); err != nil {
			logError("Shortcut creation failed: %v", err)
			dialog.ShowError(err, parentWin)
			return
		}
		dlg.Hide()
		logInfo("Created shortcut for %s", a.Name)
		dialog.ShowInformation("Shortcut Created",
			fmt.Sprintf("Desktop shortcut for '%s' created.\n\nRemember: do not move the launcher folder.", a.Name),
			parentWin)
	}
	cancelBtn.OnTapped = func() { dlg.Hide() }
	dlg.Show()
}

// ---- GUI ----

func buildUI(fyneApp fyne.App, config *Config, advCfg AdvancedConfig) fyne.Window {
	w := fyneApp.NewWindow(appDisplayName)
	w.Resize(fyne.NewSize(600, 580))
	w.SetMaster()

	// showExitConfirm performs the actual exit after optional confirmation
	doExit := func() {
		logInfo("pwaWarden exiting — restoring tray-hidden PWAs and unhooking")

		// Show a brief "Closing..." dialog — gives Windows time to process
		// the tray icon removal before the process exits
		closingLabel := widget.NewLabel("Closing " + appDisplayName + "...")
		closingLabel.Alignment = fyne.TextAlignCenter
		closingDlg := dialog.NewCustomWithoutButtons("", container.NewPadded(closingLabel), w)
		closingDlg.Show()

		go func() {
			if wardenTray != nil && wardenTray.threadID != 0 {
				procPostThreadMessage.Call(uintptr(wardenTray.threadID), wmWardenRemoveTray, 0, 0)
			}
			time.Sleep(800 * time.Millisecond)

			trayWindowsMu.Lock()
			for _, s := range trayWindows {
				if s.isHidden {
					logInfo("Restoring tray-hidden PWA before exit: %s", s.appName)
					removeTrayIcon(s)
					procShowWindow.Call(s.hwnd, swRestore)
					procSetForegroundWindow.Call(s.hwnd)
				}
				if s.hookHandle != 0 {
					procUnhookWinEvent.Call(s.hookHandle)
				}
				if s.hookHandle2 != 0 {
					procUnhookWinEvent.Call(s.hookHandle2)
				}
			}
			trayWindowsMu.Unlock()
			os.Exit(0)
		}()
	}

	confirmExit := func() {
		var allRunning, trayRunning []string
		for _, a := range config.Apps {
			if isPWARunning(a.AppID) {
				allRunning = append(allRunning, a.Name)
				if a.TrayMode != TrayModeOff && a.TrayMode != "" {
					trayRunning = append(trayRunning, a.Name)
				}
			}
		}

		// No PWAs running — exit immediately
		if len(allRunning) == 0 {
			removeWardenTrayIcon()
			doExit()
			return
		}

		// PWAs running with tray mode — warn that tray behavior will stop
		if len(trayRunning) > 0 {
			label := widget.NewLabel(fmt.Sprintf(
				"The following app(s) are configured to minimize to the system tray:\n\n• %s\n\nClosing %s means tray behavior will no longer work for these apps.",
				strings.Join(trayRunning, "\n• "),
				appDisplayName,
			))
			label.Wrapping = fyne.TextWrapWord
			exitBtn := widget.NewButtonWithIcon("Close Anyway", theme.LogoutIcon(), nil)
			exitBtn.Importance = widget.DangerImportance
			cancelBtn := widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), nil)
			dlg := dialog.NewCustomWithoutButtons("Tray Apps Still Running", container.NewBorder(
				nil,
				container.NewGridWithColumns(2, exitBtn, cancelBtn),
				nil, nil,
				container.NewPadded(label),
			), w)
			dlg.Resize(fyne.NewSize(440, 240))
			cancelBtn.OnTapped = func() { dlg.Hide() }
			exitBtn.OnTapped = func() { dlg.Hide(); removeWardenTrayIcon(); doExit() }
			dlg.Show()
			return
		}

		// PWAs running but none need tray — simple confirmation, leave them running
		if advCfg.SkipExitConfirmation {
			removeWardenTrayIcon()
			doExit()
			return
		}
		label := widget.NewLabel(fmt.Sprintf(
			"The following app(s) are still running:\n\n• %s\n\nExit %s?",
			strings.Join(allRunning, "\n• "),
			appDisplayName,
		))
		label.Wrapping = fyne.TextWrapWord
		dontAskCheck := widget.NewCheck("Don't ask again", nil)
		exitBtn := widget.NewButtonWithIcon("Exit", theme.LogoutIcon(), nil)
		exitBtn.Importance = widget.DangerImportance
		cancelBtn := widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), nil)
		dlg := dialog.NewCustomWithoutButtons("Exit?", container.NewBorder(
			nil,
			container.NewVBox(
				container.NewCenter(dontAskCheck),
				container.NewGridWithColumns(2, exitBtn, cancelBtn),
			),
			nil, nil,
			container.NewPadded(label),
		), w)
		dlg.Resize(fyne.NewSize(380, 220))
		cancelBtn.OnTapped = func() { dlg.Hide() }
		exitBtn.OnTapped = func() {
			dlg.Hide()
			if dontAskCheck.Checked {
				setSkipExitConfirmation(baseDir)
				advCfg.SkipExitConfirmation = true
			}
			removeWardenTrayIcon()
			doExit()
		}
		dlg.Show()
	}

	// Minimize to tray instead of minimizing to taskbar
	w.SetCloseIntercept(func() {
		confirmExit()
	})

	var appList *widget.List

	appList = widget.NewList(
		func() int { return len(config.Apps) },
		func() fyne.CanvasObject {
			return widget.NewLabel("template")
		},
		func(id widget.ListItemID, item fyne.CanvasObject) {
			item.(*widget.Label).SetText(config.Apps[id].Name)
		},
	)

	selectedIndex := -1
	appList.OnSelected = func(id widget.ListItemID) { selectedIndex = id }
	appList.OnUnselected = func(id widget.ListItemID) { selectedIndex = -1 }

	if len(config.Apps) > 0 {
		selectedIndex = 0
		appList.Select(0)
	}

	launchBtn := widget.NewButtonWithIcon("Launch", theme.MediaPlayIcon(), func() {
		if selectedIndex < 0 || selectedIndex >= len(config.Apps) {
			dialog.ShowInformation("No Selection", "Please select an app to launch.", w)
			return
		}
		a := config.Apps[selectedIndex]

		if !advCfg.AllowMultiplePWAInstances {
			pids := findChromeProcessesWithAppID(a.AppID)
			if len(pids) > 0 {
				if sess := findSessionByAppID(a.AppID); sess != nil && sess.isHidden {
					showTrayWindow(sess)
					return
				}
				pidStrs := make([]string, len(pids))
				for i, p := range pids {
					pidStrs[i] = fmt.Sprintf("%d", p)
				}
				label := widget.NewLabel(fmt.Sprintf(
					"'%s' already has %d running Chrome process(es) (PID: %s).\n\nThis may be a stray background process.\n\nKill and launch fresh, or cancel?",
					a.Name, len(pids), strings.Join(pidStrs, ", "),
				))
				label.Wrapping = fyne.TextWrapWord
				killBtn := widget.NewButtonWithIcon("Kill & Launch Fresh", theme.MediaPlayIcon(), nil)
				killBtn.Importance = widget.HighImportance
				cancelBtn := widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), nil)
				dlg := dialog.NewCustomWithoutButtons("Already Running", container.NewBorder(
					nil, container.NewGridWithColumns(2, killBtn, cancelBtn), nil, nil,
					container.NewPadded(label),
				), w)
				dlg.Resize(fyne.NewSize(460, 220))
				cancelBtn.OnTapped = func() { dlg.Hide() }
				killBtn.OnTapped = func() {
					dlg.Hide()
					killChromeProcessesWithAppID(a.AppID, a.Name)
					if err := launchPWA(config, a); err != nil {
						dialog.ShowError(err, w)
					}
				}
				dlg.Show()
				return
			}
		}

		if err := launchPWA(config, a); err != nil {
			logError("Launch failed: %v", err)
			dialog.ShowError(err, w)
			return
		}
		logInfo("Launched: %s", a.Name)
	})
	launchBtn.Importance = widget.HighImportance

	editBtn := widget.NewButtonWithIcon("Settings", theme.SettingsIcon(), func() {
		if selectedIndex < 0 || selectedIndex >= len(config.Apps) {
			dialog.ShowInformation("No Selection", "Please select an app to edit.", w)
			return
		}
		showEditDialog(w, config, selectedIndex, appList, advCfg)
	})

	shortcutBtn := widget.NewButtonWithIcon("Desktop Shortcut", theme.FileIcon(), func() {
		if selectedIndex < 0 || selectedIndex >= len(config.Apps) {
			dialog.ShowInformation("No Selection", "Please select an app to create a shortcut for.", w)
			return
		}
		showShortcutDialog(w, config, selectedIndex)
	})

	removeBtn := widget.NewButtonWithIcon("Remove", theme.DeleteIcon(), func() {
		if selectedIndex < 0 || selectedIndex >= len(config.Apps) {
			dialog.ShowInformation("No Selection", "Please select an app to remove.", w)
			return
		}
		name := config.Apps[selectedIndex].Name

		label := widget.NewLabel(fmt.Sprintf("Remove '%s' from the launcher?\n\nThis only removes it from this launcher — it does not uninstall the PWA from Chrome.", name))
		label.Wrapping = fyne.TextWrapWord

		removeBtn2 := widget.NewButtonWithIcon("Remove", theme.DeleteIcon(), nil)
		removeBtn2.Importance = widget.DangerImportance
		cancelBtn := widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), nil)
		buttons := container.NewGridWithColumns(2, removeBtn2, cancelBtn)
		content := container.NewBorder(nil, buttons, nil, nil, container.NewPadded(label))

		dlg := dialog.NewCustomWithoutButtons("Remove App", content, w)
		dlg.Resize(fyne.NewSize(400, 180))
		cancelBtn.OnTapped = func() { dlg.Hide() }
		removeBtn2.OnTapped = func() {
			dlg.Hide()
			config.Apps = append(config.Apps[:selectedIndex], config.Apps[selectedIndex+1:]...)
			selectedIndex = -1
			if err := saveConfig(config); err != nil {
				logError("Save failed: %v", err)
				dialog.ShowError(err, w)
			}
			appList.Refresh()
			logInfo("Removed: %s", name)
		}
		dlg.Show()
	})

	setupBtn := widget.NewButtonWithIcon("Open Chrome for Setup", theme.ComputerIcon(), func() {
		if err := launchSetup(config); err != nil {
			logError("Setup failed: %v", err)
			dialog.ShowError(err, w)
			return
		}
		dialog.ShowInformation("Setup",
			"Chrome is open.\n\nInstall your PWAs using Chrome's install button,\nthen close Chrome and click 'Scan for PWAs'.", w)
	})

	scanBtn := widget.NewButtonWithIcon("Scan for PWAs", theme.SearchIcon(), func() {
		pwas, err := scanPWAs()
		if err != nil {
			logError("Scan failed: %v", err)
			dialog.ShowError(err, w)
			return
		}
		if len(pwas) == 0 {
			dialog.ShowInformation("No PWAs Found",
				"No installed PWAs detected.\nMake sure you installed PWAs in Chrome during Setup.", w)
			return
		}

		var newPWAs []PWAInfo
		for _, p := range pwas {
			exists := false
			for _, a := range config.Apps {
				if a.AppID == p.AppID {
					exists = true
					break
				}
			}
			if !exists {
				newPWAs = append(newPWAs, p)
			}
		}
		if len(newPWAs) == 0 {
			dialog.ShowInformation("Up to Date", "All detected PWAs are already configured.", w)
			return
		}

		newNames := make([]string, len(newPWAs))
		for i, p := range newPWAs {
			newNames[i] = p.Name
		}

		selectWidget := widget.NewSelect(newNames, nil)
		selectWidget.PlaceHolder = "Select a PWA..."
		if len(newNames) > 0 {
			selectWidget.SetSelected(newNames[0])
		}
		ignoreCertCheck := widget.NewCheck("Ignore certificate errors", nil)
		flagsEntry := widget.NewEntry()
		flagsEntry.SetPlaceHolder("Extra flags (space separated, optional)")

		formContent := container.NewVBox(
			container.NewGridWithColumns(2, widget.NewLabel("PWA"), selectWidget),
			container.NewGridWithColumns(2, widget.NewLabel("Options"), ignoreCertCheck),
			container.NewGridWithColumns(2, widget.NewLabel("Flags"), flagsEntry),
		)

		addBtn := widget.NewButtonWithIcon("Add", theme.ContentAddIcon(), nil)
		addBtn.Importance = widget.HighImportance
		cancelBtn := widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), nil)
		buttons := container.NewGridWithColumns(2, addBtn, cancelBtn)
		content := container.NewBorder(nil, buttons, nil, nil, container.NewPadded(formContent))

		dlg := dialog.NewCustomWithoutButtons("Add PWA", content, w)
		dlg.Resize(fyne.NewSize(440, 220))
		cancelBtn.OnTapped = func() { dlg.Hide() }
		addBtn.OnTapped = func() {
			if selectWidget.Selected == "" {
				return
			}
			var chosen PWAInfo
			for _, p := range newPWAs {
				if p.Name == selectWidget.Selected {
					chosen = p
					break
				}
			}
			var flags []string
			if f := strings.TrimSpace(flagsEntry.Text); f != "" {
				flags = strings.Fields(f)
			}
			newApp := AppEntry{
				Name:       chosen.Name,
				AppID:      chosen.AppID,
				IgnoreCert: ignoreCertCheck.Checked,
				Flags:      flags,
			}
			config.Apps = append(config.Apps, newApp)
			if err := saveConfig(config); err != nil {
				logError("Save failed: %v", err)
				dialog.ShowError(err, w)
				return
			}
			appList.Refresh()
			logInfo("Added: %s (%s)", chosen.Name, chosen.AppID)
			dlg.Hide()
			dialog.ShowInformation("Added", fmt.Sprintf("'%s' has been added.", chosen.Name), w)
		}
		dlg.Show()
	})

	header := container.NewVBox(
		widget.NewLabelWithStyle(appDisplayName, fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		widget.NewLabelWithStyle("Select an app to launch or manage", fyne.TextAlignCenter, fyne.TextStyle{Italic: true}),
		container.NewGridWithColumns(2, setupBtn, scanBtn),
	)

	exitBtn := widget.NewButtonWithIcon("Exit", theme.LogoutIcon(), func() {
		confirmExit()
	})

	footer := container.NewGridWithColumns(5, launchBtn, removeBtn, editBtn, shortcutBtn, exitBtn)
	w.SetContent(container.NewBorder(header, footer, nil, nil, appList))

	// Start minimize-to-tray after the window is shown.
	// We use a WinEventHook on our own process to catch minimize events,
	// then hide the window and show a tray icon instead.
	go func() {
		// Wait for the Fyne window HWND to appear — search by title
		var mainHWND uintptr
		titlePtr, _ := syscall.UTF16PtrFromString(appDisplayName)
		for i := 0; i < 80; i++ {
			time.Sleep(100 * time.Millisecond)
			// Search all top-level windows for our title
			hwnd, _, _ := procFindWindowEx.Call(0, 0, 0, uintptr(unsafe.Pointer(titlePtr)))
			if hwnd != 0 {
				mainHWND = hwnd
				logInfo("pwaWarden HWND found: 0x%X", mainHWND)
				break
			}
		}
		if mainHWND == 0 {
			logError("Could not find pwaWarden window handle for tray minimize")
			return
		}

		// Start the warden tray message loop in another goroutine
		go startWardenTray(mainHWND, func() {
			confirmExit()
		})

		// Wait for wardenTray to be initialized then add persistent icon
		for i := 0; i < 20; i++ {
			time.Sleep(50 * time.Millisecond)
			if wardenTray != nil {
				break
			}
		}
		addWardenTrayIcon(mainHWND)

		// Use WinEventHook to catch minimize on our own window
		// Continuously save placement while window is visible
		// so we always have a good non-minimized position to restore to
		go func() {
			for {
				time.Sleep(500 * time.Millisecond)
				if wardenTray == nil {
					continue
				}
				var placement windowPlacement
				placement.Length = uint32(unsafe.Sizeof(placement))
				procGetWindowPlacement.Call(mainHWND, uintptr(unsafe.Pointer(&placement)))
				// Only save when not minimized or hidden
				if placement.ShowCmd != 2 && placement.ShowCmd != 0 {
					wardenTray.savedPlacement = placement
				}
			}
		}()

		hideToTray := func() {
			logInfo("pwaWarden minimized — hiding to tray")
			procShowWindow.Call(mainHWND, swHide)
		}

		pid := uint32(os.Getpid())
		cb := syscall.NewCallback(func(hHook, event uintptr, hwnd uintptr, idObj, idChild int32, thread, ms uint32) uintptr {
			if hwnd != mainHWND {
				return 0
			}
			switch event {
			case eventMinimizeStart:
				hideToTray()
			}
			return 0
		})

		hook, _, _ := procSetWinEventHook.Call(
			eventMinimizeStart, eventMinimizeStart, 0, cb,
			uintptr(pid), 0, winEventOutOfContext,
		)
		if hook == 0 {
			logError("WinEventHook for pwaWarden minimize failed — falling back to polling")
			// Fallback: poll GetWindowPlacement
			wasMinimized := false
			for {
				time.Sleep(100 * time.Millisecond)
				placement := windowPlacement{}
				placement.Length = uint32(unsafe.Sizeof(placement))
				procGetWindowPlacement.Call(mainHWND, uintptr(unsafe.Pointer(&placement)))
				isMinimized := placement.ShowCmd == 2
				if isMinimized && !wasMinimized {
					hideToTray()
					wasMinimized = true
				} else if !isMinimized {
					wasMinimized = false
				}
			}
		}

		// Pump messages for the hook on this goroutine's thread
		var msg winMSG
		for {
			ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if ret == 0 || ret == ^uintptr(0) {
				break
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
		}
		procUnhookWinEvent.Call(hook)
	}()

	return w
}

// ---- Main ----


func main() {
	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Could not determine executable path:", err)
		os.Exit(1)
	}
	baseDir = filepath.Dir(exePath)
	initLogger()
	logInfo("pwaWarden starting — base dir: %s", baseDir)

	advCfg := loadAdvancedConfig(baseDir)

	if !advCfg.AllowMultiplePwaWardenInstances {
		mutexName, _ := syscall.UTF16PtrFromString(appMutexName)
		const errorAlreadyExists = uintptr(183)
		hMutex, _, callErr := procCreateMutex.Call(0, 1, uintptr(unsafe.Pointer(mutexName)))
		if hMutex == 0 || uintptr(callErr.(syscall.Errno)) == errorAlreadyExists {
			logError("Another instance already running")
			fyneApp := app.New()
			fyneApp.SetIcon(fyne.NewStaticResource("icon", appIconPNG))
			w := fyneApp.NewWindow(appDisplayName)
			w.Resize(fyne.NewSize(420, 160))
			w.SetFixedSize(true)
			centerWindow(w)
			label := widget.NewLabel(appDisplayName + " is already running.\n\nCheck your system tray or taskbar.")
			label.Alignment = fyne.TextAlignCenter
			okBtn := widget.NewButtonWithIcon("OK", theme.ConfirmIcon(), func() { fyneApp.Quit() })
			okBtn.Importance = widget.HighImportance
			w.SetContent(container.NewBorder(nil, container.NewPadded(container.NewCenter(okBtn)), nil, nil, container.NewCenter(label)))
			w.ShowAndRun()
			return
		}
		logInfo("Single-instance mutex acquired (handle=0x%X)", hMutex)
	}

	if len(os.Args) > 1 {
		appName := os.Args[1]
		config := loadConfig()

		fyneApp := app.New()
		w := fyneApp.NewWindow(appDisplayName)
		w.Resize(fyne.NewSize(400, 200))
		centerWindow(w)

		warned := false
		for _, a := range config.Apps {
			if strings.EqualFold(a.Name, appName) {
				if !warned {
					warned = true
					msg := fmt.Sprintf(
						"⚠️  Launching via desktop shortcut.\n\nThis shortcut is hardcoded to:\n%s\n\nIf you move the launcher, update or recreate the shortcut.",
						filepath.Dir(exePath),
					)
					d := dialog.NewInformation("Shortcut Launch", msg, w)
					d.SetOnClosed(func() {
						if err := launchPWA(config, a); err != nil {
							logError("Shortcut launch failed: %v", err)
							dialog.ShowError(err, w)
							return
						}
						logInfo("Shortcut launched: %s", a.Name)
						fyneApp.Quit()
					})
					w.Show()
					centerWindow(w)
					d.Show()
					fyneApp.Run()
					return
				}
			}
		}
		logError("App not found for shortcut launch: %s", appName)
		fyneApp2 := app.New()
		w2 := fyneApp2.NewWindow("Error")
		w2.Resize(fyne.NewSize(360, 120))
		centerWindow(w2)
		w2.SetContent(widget.NewLabel(fmt.Sprintf("App '%s' not found in config.\nOpen the launcher and run Scan for PWAs.", appName)))
		w2.ShowAndRun()
		return
	}

	config := loadConfig()
	fyneApp := app.New()
	fyneApp.SetIcon(fyne.NewStaticResource("icon", appIconPNG))

	setupWin := fyneApp.NewWindow(appDisplayName + " — Setup")
	setupWin.Resize(fyne.NewSize(540, 280))
	setupWin.SetFixedSize(true)

	go func() {
		centerWindow(setupWin)
		chromePath := filepath.Join(baseDir, "app", "chrome.exe")
		if _, err := os.Stat(chromePath); err == nil {
			logInfo("Chrome binary found at %s", chromePath)
			setupWin.Close()
			w := buildUI(fyneApp, config, advCfg)
			w.Show()
			centerWindow(w)
			return
		}

		logInfo("Chrome binary not found — prompting user")

		msg := fmt.Sprintf(
			"Chrome not found at:\n%s\n\nWould you like to automatically download and install Ungoogled Chromium Portable (win64) from GitHub?",
			chromePath,
		)

		msgLabel := widget.NewLabel(msg)
		msgLabel.Wrapping = fyne.TextWrapWord

		downloadBtn := widget.NewButtonWithIcon("Download", theme.DownloadIcon(), nil)
		downloadBtn.Importance = widget.HighImportance
		cancelBtn := widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), nil)
		buttons := container.NewGridWithColumns(2, downloadBtn, cancelBtn)
		promptContent := container.NewBorder(nil, buttons, nil, nil, container.NewPadded(msgLabel))

		promptDlg := dialog.NewCustomWithoutButtons("Chrome Not Found", promptContent, setupWin)
		promptDlg.Resize(fyne.NewSize(480, 220))
		cancelBtn.OnTapped = func() {
			promptDlg.Hide()
			logInfo("User declined Chrome download")
			fyneApp.Quit()
		}
		downloadBtn.OnTapped = func() {
			promptDlg.Hide()

			progressLabel := widget.NewLabel("Fetching latest release info…")
			progressBar := widget.NewProgressBar()
			progressBar.Min = 0
			progressBar.Max = 1
			dlContent := container.NewVBox(progressLabel, progressBar)
			dlDialog := dialog.NewCustomWithoutButtons("Downloading Chrome", dlContent, setupWin)
			dlDialog.Show()

			go func() {
				downloadURL, filename, fetchErr := fetchLatestChromeRelease()
				if fetchErr != nil {
					logError("Release fetch failed: %v", fetchErr)
					dlDialog.Hide()
					dialog.ShowError(fmt.Errorf("Could not fetch release info:\n%v", fetchErr), setupWin)
					return
				}

				tmpDir := filepath.Join(baseDir, "tmp")
				os.MkdirAll(tmpDir, 0755)
				archivePath := filepath.Join(tmpDir, filename)

				progressLabel.SetText(fmt.Sprintf("Downloading %s…", filename))
				dlErr := downloadFile(downloadURL, archivePath, func(downloaded, total int64) {
					if total > 0 {
						progressBar.SetValue(float64(downloaded) / float64(total))
						progressLabel.SetText(fmt.Sprintf("Downloading… %.1f / %.1f MB", float64(downloaded)/1e6, float64(total)/1e6))
					}
				})
				if dlErr != nil {
					logError("Download failed: %v", dlErr)
					dlDialog.Hide()
					dialog.ShowError(fmt.Errorf("Download failed:\n%v", dlErr), setupWin)
					return
				}

				progressLabel.SetText("Extracting…")
				if exErr := extract7z(archivePath, baseDir, func(status string) {
					progressLabel.SetText("Extracting… " + status)
				}); exErr != nil {
					logError("Extraction failed: %v", exErr)
					dlDialog.Hide()
					dialog.ShowError(fmt.Errorf("Extraction failed:\n%v", exErr), setupWin)
					return
				}

				if _, statErr := os.Stat(chromePath); statErr != nil {
					logError("chrome.exe not found after extraction: %v", statErr)
					dlDialog.Hide()
					dialog.ShowError(fmt.Errorf("chrome.exe not found at:\n%s", chromePath), setupWin)
					return
				}

				logInfo("chrome.exe verified")
				dlDialog.Hide()

				d := dialog.NewInformation("Chrome Ready", "Ungoogled Chromium Portable installed.\n\nThe launcher will now open.", setupWin)
				d.SetOnClosed(func() {
					setupWin.Close()
					w := buildUI(fyneApp, config, advCfg)
					w.Show()
					centerWindow(w)
				})
				d.Show()
			}()
		}
		promptDlg.Show()
	}()

	setupWin.ShowAndRun()
}