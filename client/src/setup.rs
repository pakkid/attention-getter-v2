//! Interactive `setup`: pair with the server, pick a monitor and hotkey.

use std::io::{BufRead, Write};

use anyhow::{Context, Result, bail};
use serde::Deserialize;
use winit::application::ApplicationHandler;
use winit::event::WindowEvent;
use winit::event_loop::{ActiveEventLoop, EventLoop};
use winit::window::WindowId;

use crate::config::{self, Config};

pub struct MonitorInfo {
    pub name: String,
    pub width: u32,
    pub height: u32,
    pub x: i32,
    pub y: i32,
    pub primary: bool,
}

impl std::fmt::Display for MonitorInfo {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "{}  {}x{} at {},{}{}",
            self.name,
            self.width,
            self.height,
            self.x,
            self.y,
            if self.primary { "  (primary)" } else { "" }
        )
    }
}

/// Lists monitors via winit (one event loop per process, so call once).
pub fn monitors() -> Result<Vec<MonitorInfo>> {
    struct Probe(Vec<MonitorInfo>);
    impl ApplicationHandler for Probe {
        fn resumed(&mut self, el: &ActiveEventLoop) {
            let primary = el.primary_monitor().and_then(|m| m.name());
            for m in el.available_monitors() {
                let (size, pos) = (m.size(), m.position());
                let name = m.name().unwrap_or_else(|| "unknown".into());
                self.0.push(MonitorInfo {
                    primary: primary.as_deref() == Some(name.as_str()),
                    name,
                    width: size.width,
                    height: size.height,
                    x: pos.x,
                    y: pos.y,
                });
            }
            el.exit();
        }
        fn window_event(&mut self, _: &ActiveEventLoop, _: WindowId, _: WindowEvent) {}
    }
    let el = EventLoop::new()?;
    let mut probe = Probe(Vec::new());
    el.run_app(&mut probe)?;
    Ok(probe.0)
}

fn prompt(label: &str, default: &str) -> Result<String> {
    if default.is_empty() {
        print!("{label}: ");
    } else {
        print!("{label} [{default}]: ");
    }
    std::io::stdout().flush()?;
    let mut s = String::new();
    std::io::stdin().lock().read_line(&mut s)?;
    let s = s.trim();
    Ok(if s.is_empty() { default.to_string() } else { s.to_string() })
}

#[derive(Deserialize)]
struct PairResp {
    token: String,
    name: String,
}

#[derive(Deserialize)]
struct ErrResp {
    error: String,
}

fn hostname() -> String {
    std::env::var("COMPUTERNAME")
        .ok()
        .or_else(|| std::fs::read_to_string("/etc/hostname").ok())
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| "my-pc".into())
}

pub fn run() -> Result<()> {
    let existing = Config::load().ok();
    println!("Attention Getter setup. Press Enter to keep the value in [brackets].\n");

    let server = prompt(
        "Server URL (e.g. https://attention.example.com)",
        existing.as_ref().map_or("", |c| c.server.as_str()),
    )?;
    if !server.starts_with("http://") && !server.starts_with("https://") {
        bail!("server URL must start with https:// (or http:// for local testing)");
    }
    let server = server.trim_end_matches('/').to_string();

    let (token, name) = match existing.as_ref().filter(|c| c.server == server) {
        Some(c) if prompt("Already paired. Pair again? (y/N)", "n")?.eq_ignore_ascii_case("n") => (c.token.clone(), c.name.clone()),
        _ => {
            println!("In the web app: Admin → PCs → + Add PC to get a pairing code.");
            let code = prompt("Pairing code", "")?;
            let name = prompt("Name for this PC", &existing.as_ref().map_or_else(hostname, |c| c.name.clone()))?;
            let rt = tokio::runtime::Builder::new_current_thread().enable_all().build()?;
            let resp = rt.block_on(async {
                let r = reqwest::Client::new()
                    .post(format!("{server}/api/device/pair"))
                    .json(&serde_json::json!({ "code": code, "name": name }))
                    .send()
                    .await?;
                if !r.status().is_success() {
                    let msg = r
                        .json::<ErrResp>()
                        .await
                        .map(|e| e.error)
                        .unwrap_or_else(|_| "pairing failed".into());
                    bail!("{msg}");
                }
                Ok(r.json::<PairResp>().await?)
            })?;
            println!("Paired as \"{}\".", resp.name);
            (resp.token, resp.name)
        }
    };

    let mons = monitors().context("listing monitors")?;
    println!("\nMonitors:");
    for (i, m) in mons.iter().enumerate() {
        println!("  {}) {m}", i + 1);
    }
    let current = existing.as_ref().map(|c| c.monitor.clone()).unwrap_or_default();
    let default_idx = mons
        .iter()
        .position(|m| m.name == current)
        .or_else(|| mons.iter().position(|m| m.primary))
        .unwrap_or(0)
        + 1;
    let pick = prompt("Show the popup on monitor #", &default_idx.to_string())?;
    let monitor = pick
        .parse::<usize>()
        .ok()
        .and_then(|i| mons.get(i.wrapping_sub(1)))
        .map(|m| m.name.clone())
        .context("invalid monitor number")?;

    let hotkey = loop {
        let h = prompt("Hotkey to focus the popup", existing.as_ref().map_or("F13", |c| c.hotkey.as_str()))?;
        match h.parse::<global_hotkey::hotkey::HotKey>() {
            Ok(_) => break h,
            Err(e) => println!("  not a valid hotkey ({e}); examples: F13, Ctrl+Shift+F12, Alt+Insert"),
        }
    };

    let cfg = Config {
        server,
        token,
        name,
        monitor,
        hotkey,
    };
    cfg.save()?;
    println!("\nSaved {}", config::config_path()?.display());
    println!("Next: `attention-getter test` to preview, then `attention-getter install` to start at login.");
    Ok(())
}
