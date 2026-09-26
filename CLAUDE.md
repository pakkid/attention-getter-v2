# Attention Getter: notes for Claude

Two independently deployed halves (see README.md for what they do):

- `server/`: Go server + the PWA in `server/web/` (embedded into the binary with `go:embed`). Runs in Docker on the user's host as a Portainer stack built from `main`. Prod: https://attention-getter.stuffandthings.cc
- `client/`: Rust PC client (Linux + Windows), shipped as GitHub release binaries and installed with the one-line scripts in `client/scripts/`.

They update on different schedules and **clients never auto-update**: a PC keeps running whatever version it last installed until someone re-runs the installer. So the server must keep working with older clients, and every client change needs a release.

## House rules

- Never add `Co-Authored-By` or any AI attribution to commits or PRs.
- Don't commit, push or tag unless the user asks. Ask before each push and each tag.
- Commit messages: short imperative subject, then a wrapped body explaining why (see `git log`).
- Before committing: `cd server && go vet ./... && go test -race ./...` and `cd client && cargo test && cargo clippy --all-targets`.

## Updating the server (incl. the web app)

1. Change code, run the server checks, commit to `main`, push (when the user says so).
2. **Pushing does not deploy.** The user must click **Pull and redeploy** on the stack in Portainer. Always remind them.
3. Run locally with `cd server && DATA_DIR=./data PUBLIC_URL=http://localhost:8080 DEV_LOGIN=1 ADMIN_EMAILS=you@example.com go run ./cmd/server` (or the `server-dev` config in `.claude/launch.json` if it exists on this machine; `.claude/` is gitignored). Web files are embedded, so **restart the server** after editing anything in `server/web/`.

Things that break if you forget them:

- **Database changes:** append a new string to `migrations` in `server/internal/store/store.go`. Never edit the base `schema` or an existing migration: `PRAGMA user_version` counts applied migrations, and fresh databases run the base schema *and then* every migration.
- **Old clients:** the WebSocket protocol (`server/internal/hub/hub.go` ↔ `client/src/protocol.rs`) must stay backward compatible. Adding JSON fields is safe (serde ignores unknown ones). A new server→client `op` is logged and ignored by old clients. Renaming or removing fields or ops, or changing `/api/device/pair`, `/api/device/ws` or `/api/device/manifest`, breaks every PC that hasn't reinstalled.
- **Caching:** `staticHandler` in `server/internal/api/api.go` serves every web file with `Cache-Control: no-cache` plus a content-hash ETag, so a redeploy shows up on the next load. Keep it that way. Without an explicit header, Cloudflare adds a 4-hour browser TTL and the old UI sticks around. `sw.js` handles push only and caches nothing.
- **Web app UI:** follows the user's "Broadsheet" design system (tokens at the top of `server/web/style.css`). Animations must switch off under `prefers-reduced-motion`. Build DOM with `el()` in `app.js`, never `innerHTML`.

## Updating the client (a release)

1. Make the change and run the client checks (`cargo test && cargo clippy --all-targets` in `client/`).
2. Bump `version` in `client/Cargo.toml` (semver), then run `cargo check` in `client/` so `Cargo.lock` picks it up. Commit both.
3. Push `main`, then tag with the **same** version and push the tag:
   ```sh
   git tag v1.2.3 && git push origin v1.2.3
   ```
4. `.github/workflows/release.yml` tests the server, builds the Docker image, builds the Linux (ubuntu-22.04, for older glibc) and Windows binaries, and publishes the GitHub release with `SHA256SUMS`. Watch it with `gh run watch`, then check the release has all three assets: `gh release view v1.2.3`.
5. Tell the user PCs update by re-running the installer (or Admin → PCs → "Install or update the PC app" in the web app). Pairing and settings are kept.

Release gotchas:

- **Every `v*` tag publishes a full, non-prerelease release, which becomes `releases/latest` and is what every installer downloads.** Don't push throwaway or `-rc` tags. To try the build without releasing, run the workflow manually (Actions → release → Run workflow, i.e. `workflow_dispatch`); it builds and tests but skips the release job.
- If a release is bad, fix it and ship a new patch version. Don't move or reuse a tag.
- A server-only change needs no tag, but a tag also runs the server tests. Server deploys still happen only through Portainer.
- Cross-compiling Windows from Linux for local testing: `client/scripts/build-windows.sh` (needs zig). CI builds the MSVC target instead.

## Keeping the install scripts working

`client/scripts/install.sh` (Linux) and `client/scripts/install.ps1` (Windows) are fetched straight from `main` on raw.githubusercontent.com. **A change to them goes live the moment it's pushed**, while they download the binary from the **latest release**. So a script must work with the currently released binary: if a script change needs a new binary, tag and publish the release first, then push the script change (or make the script handle both).

The scripts depend on these contracts. If you change one side, change the other in the same commit:

| Contract | Defined in | Relied on by |
|---|---|---|
| Release asset names `attention-getter-linux-x86_64`, `attention-getter-windows-x86_64.exe`, and `SHA256SUMS` in `sha256sum` format (`<hash>  <name>`) | `release.yml` (matrix `asset`, Checksums step) | both scripts (download URL + checksum grep) |
| Repo `pakkid/attention-getter-v2` and paths `client/scripts/install.{sh,ps1}` on `main` | the repo | `repo=` in both scripts, the `INSTALL` commands in `server/web/app.js`, README "Quick install" |
| Subcommands: `setup` (interactive; writes the config), `install` (idempotent; enables autostart and (re)starts the service), `run` (the service) | `client/src/main.rs`, `setup.rs`, `install.rs` | both scripts call `setup` if there's no config, then `install` |
| Config path: Linux `${XDG_CONFIG_HOME:-~/.config}/attention-getter/config.toml`, Windows `%APPDATA%\attention-getter\config\config.toml` (`directories::ProjectDirs::from("", "", "attention-getter")`) | `client/src/config.rs` | both scripts check it to decide whether to run `setup` |
| Linux unit `~/.config/systemd/user/attention-getter.service` with the line `ExecStart="<exe>" run` | `install.rs` (linux `install()`) | `install.sh` sed-parses that exact line to find and remove an old copy elsewhere |
| Windows autostart `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, value `AttentionGetter` = `"<exe>" run` | `install.rs` (windows `RUN_KEY`/`VALUE`) | `install.ps1` reads the quoted path to find the old copy and processes to stop |
| Process/exe name `attention-getter*` | Cargo package name / asset names | `install.ps1` stops running copies by name before replacing the exe |
| GNOME extension dir `~/.local/share/gnome-shell/extensions/attention-getter@pakkid/extension.js` | `install.rs` (`EXT_UUID`) | `install.sh` hashes it to tell the user to log out when it changed |
| Install locations `~/.local/bin/attention-getter`, `%LOCALAPPDATA%\Programs\attention-getter.exe` | the scripts | README, the web app's install guide text |

Other things to keep in mind:

- **Setup needs a terminal.** On Linux the script is piped from curl, so `setup` reads `/dev/tty`. On Windows the exe is a GUI-subsystem program, so the script starts it with `Start-Process -Wait`. Keep `setup` usable both ways.
- **Updates replace a running binary.** `install.sh` stops the systemd unit before `mv`, and `install.ps1` kills every copy (service and popup) before `Move-Item`, then runs `Unblock-File`. `install` itself restarts the service. Keep `install` safe to re-run on an already installed PC.
- **GNOME extension:** the files in `client/gnome-extension/` are compiled into the binary (`include_str!` in `install.rs`), so changing them needs a client release. When GNOME ships a new major version, add it to `shell-version` in `metadata.json` or GNOME won't load the extension. New settings keys go in the `.gschema.xml` (`install` runs `glib-compile-schemas`). GNOME only loads a new or changed extension after a log out and back in.
- **Pinning:** both scripts honour `AG_VERSION=vX.Y.Z`. Keep that working, since it's the documented way to roll a PC back.
- **Test a script change** on a real machine (or at least run `sh -n client/scripts/install.sh`) before pushing, because it's live instantly. To test against a specific release: `curl -fsSL .../install.sh | AG_VERSION=v1.0.0 sh`.
- If you add a platform or architecture, you need a new matrix entry and asset name in `release.yml`, a branch in the scripts (the Linux script currently refuses anything but x86_64), and entries in README and the web app's `INSTALL` table.
