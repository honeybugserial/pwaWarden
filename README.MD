# pwaWarden

**A PWA creator, launcher, manager, dictator, executioner and friend for Windows — with system tray support and more, powered by Ungoogled Chromium.**

pwaWarden makes it easy to turn any website into a proper desktop app using PWA installation — without touching your system, Chrome, registry, or OS configuration. Everything runs from a single self-contained folder: Chromium, your PWA data, settings, and pwaWarden itself.

On top of that, pwaWarden adds functionality that PWAs don't get out of the box, like system tray support — so your apps can minimize or close to the tray just like native desktop applications. Pick it up and move the folder anywhere, and everything comes with it.

---

| | |
|---|---|
| ![](https://raw.githubusercontent.com/honeybugserial/pwaWarden/main/pwaWarden-ScreenShot01.png) | ![](https://raw.githubusercontent.com/honeybugserial/pwaWarden/main/pwaWarden-ScreenShot02.png) |

---

## Features

- **Launch PWAs** as standalone windows using a portable Ungoogled Chromium install
- **System tray integration** — minimize and/or close PWAs to the tray instead of closing them
- **Per-app tray modes:** minimize to tray, close to tray, both, or off
- **Per-app settings** — custom Chrome flags, ignore certificate errors (useful for self-signed HTTPS), tray behavior
- **Desktop shortcut creation** per PWA (not portable)
- **Single-instance enforcement** — only one pwaWarden can run at a time
- **Advanced overrides** via `advanced.ini` for power users
- **Fully portable** — everything lives in one folder, nothing written to the registry

---

## Requirements

- Windows 10 or later (64-bit)
- No system Chrome required — pwaWarden downloads and manages portapps.io portable Chromium

---

## Setup

1. Download or build `pwaWarden.exe` and place it in a dedicated folder (e.g. `A:\pwaWarden\`)
2. Launch `pwaWarden.exe`
3. On first run, pwaWarden will automatically download and configure the latest portable Ungoogled Chromium from portapps.io

### Adding PWAs

1. Click **Open Chrome for Setup** to launch Chromium in regular browser mode
2. Navigate to the site you want and install it as a PWA via Chrome's menu (Install page as app...)
3. Close Chrome when done
4. Click **Scan for PWAs** — pwaWarden will detect all installed PWAs and populate the list

---

## Folder Structure

```
pwaWarden-WorkDir/
├── pwaWarden.exe
├── config.json          # PWA list and settings (auto-created and managed)
├── advanced.ini         # Optional power-user overrides
├── app/
│   └── chrome.exe       # Portable Ungoogled Chromium (auto downloaded and configured during first run)
├── data/                # Chrome user data directory (profiles, PWA icons, etc.)
└── log/
    └── launcher-YYYY-MM-DD.log
```

---

## Per-App Settings

| Setting | Description |
|---|---|
| **Name** | Display name shown in pwaWarden and the tray |
| **App ID** | Chrome PWA App ID |
| **Tray mode** | `Off`, `Minimize to tray`, `Close to tray`, or `Both` |
| **Ignore certificate errors** | Useful for PWAs served over self-signed HTTPS |
| **Extra flags** | Add any standard additional Chromium flags, space-separated |

---

## Tray Modes

| Mode | Behavior |
|---|---|
| **Off** | Normal window behavior — minimize and close work as usual |
| **Minimize to tray** | Minimizing hides the window and adds a tray icon |
| **Close to tray** | Clicking X hides the window and adds a tray icon |
| **Both** | Either minimize or close sends the PWA to the tray |

Left-click a tray icon to restore the window. Right-click for a menu with restore and abortion options.

---

## Advanced Configuration

Create a file named `advanced.ini` in the same folder as `pwaWarden.exe` to override default behavior. All keys are optional.

```ini
; pwaWarden - advanced.ini

allowMultiplePwaWardenInstances = true  ; WARNING: instances fight over tray/process tracking
allowMultiplePWAInstances       = true  ; WARNING: causes duplicate/stuck background processes
allowSettingsWhileRunning       = true  ; WARNING: changes won't apply until PWA is restarted
skipExitConfirmation            = true  ; Skip the exit confirmation dialog (set automatically)
```

> `skipExitConfirmation` is written automatically when you check "Don't ask again" in the exit dialog. You generally don't need to set it manually.

---

## Building from Source

```bash
# Install dependencies
go mod init pwaWarden
go get fyne.io/fyne/v2
go get github.com/go-ole/go-ole

# Generate version/icon resource (re-run if icon changes)
go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest
goversioninfo -64 -o resource.syso

# Build
go build -o pwaWarden.exe -ldflags="-H windowsgui"
```

Requires Go 1.21+ and a Windows build environment (or cross-compilation targeting `GOOS=windows`).

---

## Notes

- pwaWarden is **Windows-only** by design — tray integration relies on Win32 APIs
- All PWA data (profile, cookies, settings) lives in the `data/` folder — fully portable
- Do not use pwaWarden while operating heavy machinery or while breast feeding. Ask your doctor if PWAs are right for you.

---

## License

MIT

