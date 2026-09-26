# Attention Getter

Get someone off their computer without walking over. A family member taps a button in a phone app (or says something to Alexa), and one monitor on their PC fills with a GIF while a sound loops. The popup appears **without taking focus**, so a game or a typing session isn't interrupted mid-keystroke. Press **F13** (configurable) to focus it, type why you're not coming or how long you'll be (or press 1–9 for a quick reply), and hit **Enter**. The reply arrives on the phone as a push notification.

```
Phone PWA ──HTTPS──┐                          ┌── WebSocket (persistent) ── PC client
Alexa POST ?key= ──┼── Cloudflare Tunnel ── server (Docker, SQLite)            │ spawns on alert
                   └── Web Push ◄─────────────┘                        fullscreen popup
```

- **server/**: Go. Serves the PWA, Google sign-in, the trigger API, Web Push and the PC WebSocket hub. Ships as a single static binary in a ~15 MB image.
- **client/**: Rust. A tiny background service (about 1 MB of private memory, idle 0% CPU) that starts a short-lived popup process for each alert. Supports Linux (GNOME Wayland via a bundled shell extension, or X11) and Windows.

## 1. Server

1. **Google OAuth client.** In [Google Cloud Console → Credentials](https://console.cloud.google.com/apis/credentials), create an *OAuth client ID* of type **Web application**. Add your public URL (e.g. `https://attention.example.com`) under **Authorized JavaScript origins**. No redirect URIs are needed.
2. **Deploy with Portainer.** Go to Stacks → Add stack → **Repository**:
   - Repository URL: `https://github.com/pakkid/attention-getter-v2`, reference `refs/heads/main`, compose path `docker-compose.yml`.
   - Environment variables: `PUBLIC_URL` (e.g. `https://attention.example.com`), `GOOGLE_CLIENT_ID`, `ADMIN_EMAILS` (comma-separated), and optionally `HOST_PORT` (default `8080`) and `VAPID_SUBJECT`.
   - Deploy. Portainer clones the repo and builds the image on the server. To update, use **Pull and redeploy** (or enable GitOps updates).

   Without Portainer: `cp .env.example .env`, fill it in, then `docker compose up -d --build`.
3. **Cloudflare Tunnel.** In the tunnel already running on the host, add a public hostname for `PUBLIC_URL` pointing at `http://localhost:8080` (or your `HOST_PORT`). The server listens on localhost only. WebSockets and SSE work through tunnels with no extra settings.

Data (SQLite database, uploaded media, VAPID keys) lives in the `ag-data` volume.

### Web app

Open the public URL and sign in with Google. Accounts in `ADMIN_EMAILS` are admins. In **Admin** you can:

- **Allowed Google accounts**: add family members. Only listed accounts can sign in.
- **Attention types**: upload a GIF (≤ 20 MB) plus a sound (mp3/ogg/wav/flac, ≤ 5 MB) and give it a name, e.g. `dinner`. A type named `default` is used when a trigger doesn't specify one.
- **PCs → + Add PC**: shows step-by-step install instructions (Windows or Linux) with a pairing code. **Rename** changes how a PC is shown and addressed; the PC keeps working without re-pairing. *Install or update the PC app* shows the same instructions for updating.
- **PC groups**: e.g. "Upstairs" with several PCs. Groups appear next to the PCs on the Trigger tab and trigger every PC in them at once. PC and group names share one namespace, so a group also works as `pc=NAME` in trigger URLs.
- **Offline PCs**: whether an alert for an offline PC waits and pops up when that PC next connects. **Off by default**: such an alert isn't sent, the sender sees "offline, not sent", and History shows it as not delivered. Turning it off also drops alerts already waiting for offline PCs.
- **Trigger keys**: create a key for Alexa or other automation (see below).
- **Quick replies**: the presets shown in the popup under keys 1–9.

**Notifications on phones**

- **iPhone/iPad (iOS 16.4+):** open the site in Safari, tap Share, choose **Add to Home Screen**, and open it from the home screen. Then go to **Settings → Enable**. iOS only allows web push for home-screen apps.
- **Android:** in Chrome, choose Install app (or Add to Home screen), then **Settings → Enable**.

Under **Settings** each user picks what to be notified about (*replies to my requests*, *every reply*, or *nothing*) and can set the name shown on the PC popup (by default their Google first name).

## 2. PC client

**Quick install** (also updates an existing install; run it again any time):

```sh
# Linux (x86_64)
curl -fsSL https://raw.githubusercontent.com/pakkid/attention-getter-v2/main/client/scripts/install.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://raw.githubusercontent.com/pakkid/attention-getter-v2/main/client/scripts/install.ps1 | iex
```

Both download the latest release, verify its checksum, replace any existing install, run `setup` if the PC isn't paired yet, and enable start at login (on GNOME, log out and back in afterwards if the extension is new or changed). To pin a version, set `AG_VERSION=v1.0.0` (Linux: `curl ... | AG_VERSION=v1.0.0 sh`; Windows: `$env:AG_VERSION = "v1.0.0"` first).

Or download `attention-getter-linux-x86_64` or `attention-getter-windows-x86_64.exe` from the [latest release](https://github.com/pakkid/attention-getter-v2/releases/latest) and put it somewhere permanent: `~/.local/bin/attention-getter` (then `chmod +x`), or on Windows e.g. `%LOCALAPPDATA%\Programs\attention-getter.exe`.

Or build it (Rust 1.95+):

```sh
cd client && cargo build --release
# Linux: cp target/release/attention-getter ~/.local/bin/
# Windows: copy target\release\attention-getter.exe to e.g. %LOCALAPPDATA%\Programs\
```

On Linux the build needs the usual windowing and audio headers (`libxkbcommon`, `wayland`, `alsa-lib`). On Windows nothing extra is needed.

To build the Windows `.exe` from Linux (uses [zig](https://ziglang.org) as the linker; no MinGW needed):

```sh
rustup target add x86_64-pc-windows-gnu
client/scripts/build-windows.sh   # → client/target/x86_64-pc-windows-gnu/release/attention-getter.exe
```

The result is a single self-contained exe for Windows 10/11.

Then:

```sh
attention-getter setup      # server URL + pairing code, pick monitor, hotkey (default F13)
attention-getter test ~/some.gif ~/some.mp3   # preview the popup locally
attention-getter install    # start at login
```

- **Linux** installs a systemd user unit (`journalctl --user -u attention-getter -f` for logs).
- **Windows** adds an entry to `HKCU\…\Run` and starts the client in the background right away (closing the terminal doesn't stop it; re-running `install` replaces a running copy). To watch its log, end `attention-getter.exe` in Task Manager and run `attention-getter run --console`.

### GNOME (Wayland)

Wayland apps can't refuse focus when they open, and they can't focus themselves from a global key. On GNOME, `install` therefore also installs a small shell extension (`attention-getter@pakkid`) that:

- hands focus back to the window you were using when the popup appears,
- keeps the popup above other windows and on every workspace,
- binds the hotkey that focuses the popup.

GNOME only loads a newly installed extension after you **log out and back in**. Then run `gnome-extensions enable attention-getter@pakkid` if `install` couldn't enable it.

F13 is often delivered as `XF86Tools` by default keymaps, so both are bound. To change the key, edit `hotkey` in `~/.config/attention-getter/config.toml` and re-run `attention-getter install`.

On X11 and Windows the client grabs the hotkey itself, only while a popup is showing.

### How it behaves

- Alerts arrive instantly over a persistent WebSocket. If the PC is offline, the alert is not sent unless an admin turned on **Admin → Offline PCs**, in which case it is delivered when the PC reconnects. An alert the PC already received is always re-sent after a brief disconnect, so a reply typed after a network blip still arrives.
- GIFs and sounds are cached locally and re-synced every 5 minutes (a cheap `304` when nothing changed), plus immediately when an admin uploads or edits a type. If an alert's type isn't cached yet, the popup shows at once and the media appears when the download finishes.
- A second trigger while a popup is open is merged into it ("Mom, Alexa"). Your one reply goes to everyone who asked.
- Keys while focused: **Enter** sends, **1–9** on an empty box fills a quick reply, **Esc** dismisses without a reply (the phone is told "Dismissed").
- The sound loops and the popup stays until you respond. A requester can cancel it from the web app.

## 3. Alexa / automation trigger

Create a key under **Admin → Trigger keys** and POST to:

```
POST https://attention.example.com/api/trigger?key=ag_XXXX&type=dinner&pc=desktop&message=Dinner%20is%20ready
```

| Parameter | Required | Meaning |
|-----------|----------|---------|
| `key`     | yes      | The trigger key. It is shown once when created; revoke it any time. |
| `type`    | no       | Type name or id. Omitted: `default`, else the first type. Ignored if the key is pinned to a type. |
| `pc`      | no       | PC name, group name, or `all`. Omitted: the only PC (required if several are paired). Ignored if the key is pinned to a PC. |
| `message` | no       | Shown in the popup (max 200 chars). |

Response: `{"ok":true,"alerts":[{"alert_id":7,"device":"desktop","merged":false,"online":true,"missed":false}]}`. `missed` means the PC was offline and the alert wasn't sent; if that's true for every target, the response is `409` with `"ok":false`. The key's name (e.g. "Alexa") is shown as the requester, and the key's creator receives reply notifications if their preference is *replies to my requests*.

## Releasing

Push a tag such as `v0.2.0`. The [release workflow](.github/workflows/release.yml) tests the server, builds its Docker image to check the Dockerfile, builds the Linux and Windows clients, and publishes a GitHub release with checksums.

## Development

```sh
# server (DEV_LOGIN enables an email-only sign-in for local testing; never expose it)
cd server && DATA_DIR=./data PUBLIC_URL=http://localhost:8080 DEV_LOGIN=1 ADMIN_EMAILS=you@gmail.com go run ./cmd/server
go test -race ./...

# client
cd client && cargo test && cargo clippy --all-targets
```

Web Push also works on `http://localhost` in desktop Chrome, so the whole flow can be tested locally.

## License

Copyright (c) 2026 Pakkid. Licensed under the [PolyForm Strict License 1.0.0](LICENSE.md): you may use the software for personal and other noncommercial purposes, but you may not modify it, redistribute it, or build other works from it. For any other use, ask for permission.
