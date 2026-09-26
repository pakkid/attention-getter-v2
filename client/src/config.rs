use std::path::PathBuf;

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Config {
    /// Server base URL, e.g. https://attention.example.com
    pub server: String,
    /// Device token issued at pairing.
    pub token: String,
    /// This PC's name as shown in the web app.
    pub name: String,
    /// Monitor name (e.g. "DP-1") the popup fills. Empty means the primary monitor.
    #[serde(default)]
    pub monitor: String,
    /// Key that focuses the popup, e.g. "F13" or "Ctrl+Shift+F12".
    #[serde(default = "default_hotkey")]
    pub hotkey: String,
}

/// Sent on every request, e.g. "attention-getter/1.1.0 (linux)". The server shows the OS and
/// version per install, which tells the two sides of a dual-boot PC apart.
pub fn user_agent() -> String {
    format!("attention-getter/{} ({})", env!("CARGO_PKG_VERSION"), std::env::consts::OS)
}

pub fn default_hotkey() -> String {
    "F13".into()
}

fn dirs() -> Result<directories::ProjectDirs> {
    directories::ProjectDirs::from("", "", "attention-getter").context("no home directory")
}

pub fn config_path() -> Result<PathBuf> {
    Ok(dirs()?.config_dir().join("config.toml"))
}

pub fn cache_dir() -> Result<PathBuf> {
    Ok(dirs()?.cache_dir().join("media"))
}

impl Config {
    pub fn load() -> Result<Config> {
        let path = config_path()?;
        let s =
            std::fs::read_to_string(&path).with_context(|| format!("reading {} (run `attention-getter setup` first)", path.display()))?;
        toml::from_str(&s).with_context(|| format!("parsing {}", path.display()))
    }

    pub fn save(&self) -> Result<()> {
        let path = config_path()?;
        std::fs::create_dir_all(path.parent().unwrap())?;
        std::fs::write(&path, toml::to_string_pretty(self)?)?;
        #[cfg(unix)]
        {
            // The file holds the device token.
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))?;
        }
        Ok(())
    }

    pub fn server(&self) -> &str {
        self.server.trim_end_matches('/')
    }

    pub fn ws_url(&self) -> String {
        let s = self.server();
        let s = if let Some(rest) = s.strip_prefix("https://") {
            format!("wss://{rest}")
        } else if let Some(rest) = s.strip_prefix("http://") {
            format!("ws://{rest}")
        } else {
            s.to_string()
        };
        format!("{s}/api/device/ws")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ws_url_follows_scheme() {
        let mut c = Config {
            server: "https://a.example/".into(),
            token: "t".into(),
            name: "pc".into(),
            monitor: String::new(),
            hotkey: default_hotkey(),
        };
        assert_eq!(c.ws_url(), "wss://a.example/api/device/ws");
        c.server = "http://localhost:8080".into();
        assert_eq!(c.ws_url(), "ws://localhost:8080/api/device/ws");
    }

    #[test]
    fn user_agent_names_version_and_os() {
        let ua = user_agent();
        assert!(
            ua.starts_with(concat!("attention-getter/", env!("CARGO_PKG_VERSION"), " (")),
            "{ua}"
        );
        assert!(ua.ends_with(&format!("({})", std::env::consts::OS)), "{ua}");
    }

    #[test]
    fn hotkey_defaults_when_missing() {
        let c: Config = toml::from_str("server='x'\ntoken='y'\nname='z'").unwrap();
        assert_eq!(c.hotkey, "F13");
        assert_eq!(c.monitor, "");
    }
}
