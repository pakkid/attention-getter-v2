//! Autostart at login (systemd user unit / HKCU Run key) and the GNOME companion extension.

#[cfg(target_os = "linux")]
pub use linux::{install, uninstall};
#[cfg(windows)]
pub use windows::{install, uninstall};

#[cfg(not(any(target_os = "linux", windows)))]
pub fn install() -> anyhow::Result<()> {
    anyhow::bail!("autostart is only implemented for Linux and Windows")
}
#[cfg(not(any(target_os = "linux", windows)))]
pub fn uninstall() -> anyhow::Result<()> {
    install()
}

/// Converts a global-hotkey string ("Ctrl+Shift+F12") to GNOME accelerators.
#[cfg_attr(not(target_os = "linux"), allow(dead_code))]
pub fn gnome_accels(hotkey: &str) -> Vec<String> {
    let mut mods = String::new();
    let mut key = String::new();
    for part in hotkey.split('+').map(str::trim) {
        match part.to_ascii_lowercase().as_str() {
            "ctrl" | "control" => mods.push_str("<Control>"),
            "shift" => mods.push_str("<Shift>"),
            "alt" | "option" => mods.push_str("<Alt>"),
            "super" | "meta" | "win" | "cmd" | "command" | "cmdorctrl" => mods.push_str("<Super>"),
            _ => {
                key = part
                    .strip_prefix("Key")
                    .or_else(|| part.strip_prefix("Digit"))
                    .unwrap_or(part)
                    .to_string();
                if key.len() == 1 {
                    key = key.to_ascii_lowercase();
                }
            }
        }
    }
    let mut out = vec![format!("{mods}{key}")];
    if key.eq_ignore_ascii_case("F13") {
        // Default keymaps often deliver F13 as XF86Tools.
        out.push(format!("{mods}XF86Tools"));
    }
    out
}

#[cfg(target_os = "linux")]
mod linux {
    use std::path::PathBuf;
    use std::process::Command;

    use anyhow::{Context, Result, bail};

    use crate::config::Config;

    const UNIT: &str = "attention-getter.service";
    const EXT_UUID: &str = "attention-getter@pakkid";
    const SCHEMA_ID: &str = "org.gnome.shell.extensions.attention-getter";
    const EXT_FILES: &[(&str, &str)] = &[
        ("metadata.json", include_str!("../gnome-extension/metadata.json")),
        ("extension.js", include_str!("../gnome-extension/extension.js")),
        (
            "schemas/org.gnome.shell.extensions.attention-getter.gschema.xml",
            include_str!("../gnome-extension/schemas/org.gnome.shell.extensions.attention-getter.gschema.xml"),
        ),
    ];

    fn home() -> Result<PathBuf> {
        std::env::var_os("HOME").map(PathBuf::from).context("HOME not set")
    }

    fn unit_path() -> Result<PathBuf> {
        let base = std::env::var_os("XDG_CONFIG_HOME")
            .map(PathBuf::from)
            .unwrap_or(home()?.join(".config"));
        Ok(base.join("systemd/user").join(UNIT))
    }

    fn ext_dir() -> Result<PathBuf> {
        let base = std::env::var_os("XDG_DATA_HOME")
            .map(PathBuf::from)
            .unwrap_or(home()?.join(".local/share"));
        Ok(base.join("gnome-shell/extensions").join(EXT_UUID))
    }

    fn is_gnome() -> bool {
        std::env::var("XDG_CURRENT_DESKTOP").is_ok_and(|d| d.to_ascii_uppercase().contains("GNOME"))
    }

    fn sh(cmd: &str, args: &[&str]) -> Result<()> {
        let st = Command::new(cmd).args(args).status().with_context(|| format!("running {cmd}"))?;
        if !st.success() {
            bail!("{cmd} {} failed ({st})", args.join(" "));
        }
        Ok(())
    }

    pub fn install() -> Result<()> {
        let cfg = Config::load()?;
        let exe = std::env::current_exe()?;
        let unit = format!(
            "[Unit]\n\
             Description=Attention Getter\n\
             After=graphical-session.target\n\
             PartOf=graphical-session.target\n\n\
             [Service]\n\
             ExecStart=\"{}\" run\n\
             Restart=on-failure\n\
             RestartSec=5\n\n\
             [Install]\n\
             WantedBy=graphical-session.target\n",
            exe.display()
        );
        let path = unit_path()?;
        std::fs::create_dir_all(path.parent().unwrap())?;
        std::fs::write(&path, unit)?;
        sh("systemctl", &["--user", "daemon-reload"])?;
        sh("systemctl", &["--user", "enable", UNIT])?;
        sh("systemctl", &["--user", "restart", UNIT])?;
        println!("Installed {} (runs {})", path.display(), exe.display());
        println!("Logs: journalctl --user -u {UNIT} -f");

        if is_gnome() {
            install_extension(&cfg)?;
        }
        Ok(())
    }

    fn install_extension(cfg: &Config) -> Result<()> {
        let dir = ext_dir()?;
        for (name, body) in EXT_FILES {
            let p = dir.join(name);
            std::fs::create_dir_all(p.parent().unwrap())?;
            std::fs::write(p, body)?;
        }
        let schemas = dir.join("schemas");
        sh("glib-compile-schemas", &[schemas.to_str().unwrap()])?;
        let accels = super::gnome_accels(&cfg.hotkey);
        let value = format!("[{}]", accels.iter().map(|a| format!("'{a}'")).collect::<Vec<_>>().join(", "));
        sh(
            "gsettings",
            &["--schemadir", schemas.to_str().unwrap(), "set", SCHEMA_ID, "focus-key", &value],
        )?;
        println!("Installed GNOME extension to {} (hotkey {value})", dir.display());
        if sh("gnome-extensions", &["enable", EXT_UUID]).is_ok() {
            println!("Extension enabled.");
        } else {
            // GNOME Shell only discovers new extensions at login on Wayland. Adding it to
            // enabled-extensions now makes it load at the next login without another step.
            pre_enable_extension()?;
            println!("Log out and back in to load the GNOME extension (it is already marked enabled).");
        }
        Ok(())
    }

    fn pre_enable_extension() -> Result<()> {
        let out = Command::new("gsettings").args(["get", "org.gnome.shell", "enabled-extensions"]).output()?;
        let current = String::from_utf8_lossy(&out.stdout);
        if current.contains(&format!("'{EXT_UUID}'")) {
            return Ok(());
        }
        let list = current.trim().trim_start_matches("@as").trim();
        let inner = list.trim_start_matches('[').trim_end_matches(']').trim();
        let value = if inner.is_empty() {
            format!("['{EXT_UUID}']")
        } else {
            format!("[{inner}, '{EXT_UUID}']")
        };
        sh("gsettings", &["set", "org.gnome.shell", "enabled-extensions", &value])
    }

    pub fn uninstall() -> Result<()> {
        let _ = sh("systemctl", &["--user", "disable", "--now", UNIT]);
        let path = unit_path()?;
        if path.exists() {
            std::fs::remove_file(&path)?;
            let _ = sh("systemctl", &["--user", "daemon-reload"]);
        }
        let dir = ext_dir()?;
        if dir.exists() {
            let _ = sh("gnome-extensions", &["disable", EXT_UUID]);
            std::fs::remove_dir_all(&dir)?;
        }
        println!("Autostart removed. Config and cache were kept.");
        Ok(())
    }
}

#[cfg(windows)]
mod windows {
    use std::os::windows::process::CommandExt;
    use std::process::Command;

    use anyhow::{Result, bail};

    const RUN_KEY: &str = r"HKCU\Software\Microsoft\Windows\CurrentVersion\Run";
    const VALUE: &str = "AttentionGetter";
    const DETACHED_PROCESS: u32 = 0x0000_0008;

    pub fn install() -> Result<()> {
        crate::config::Config::load()?;
        let exe = std::env::current_exe()?;
        let cmd = format!("\"{}\" run", exe.display());
        let st = Command::new("reg")
            .args(["add", RUN_KEY, "/v", VALUE, "/t", "REG_SZ", "/d", &cmd, "/f"])
            .status()?;
        if !st.success() {
            bail!("reg add failed");
        }
        Command::new(&exe).arg("run").creation_flags(DETACHED_PROCESS).spawn()?;
        println!("Starts at login ({cmd}) and is running now.");
        Ok(())
    }

    pub fn uninstall() -> Result<()> {
        let _ = Command::new("reg").args(["delete", RUN_KEY, "/v", VALUE, "/f"]).status();
        println!("Autostart removed. Stop the running copy from Task Manager, or sign out.");
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::gnome_accels;

    #[test]
    fn converts_hotkeys() {
        assert_eq!(gnome_accels("F13"), vec!["F13", "XF86Tools"]);
        assert_eq!(gnome_accels("Ctrl+Shift+F12"), vec!["<Control><Shift>F12"]);
        assert_eq!(gnome_accels("Alt+KeyA"), vec!["<Alt>a"]);
        assert_eq!(gnome_accels("super+Insert"), vec!["<Super>Insert"]);
    }
}
