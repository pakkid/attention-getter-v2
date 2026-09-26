//! `update [vX.Y.Z]`: re-runs the one-line installer, which downloads the latest (or the given)
//! release, checks it, replaces this binary and restarts the service. Pairing and settings are
//! kept. The installer script stays the one place that knows how to install.

use anyhow::{Result, bail};

const SCRIPTS: &str = "https://raw.githubusercontent.com/pakkid/attention-getter-v2/main/client/scripts";

/// Accepts "1.2.3" or "v1.2.3" and returns the tag, "v1.2.3".
fn tag(version: &str) -> Result<String> {
    let v = version.trim().trim_start_matches('v');
    let ok = v.split('.').count() == 3 && v.split('.').all(|p| !p.is_empty() && p.bytes().all(|b| b.is_ascii_digit()));
    if !ok {
        bail!("{version:?} is not a version; use e.g. v1.2.3");
    }
    Ok(format!("v{v}"))
}

pub fn run(version: Option<&str>) -> Result<()> {
    let pin = version.map(tag).transpose()?;
    match &pin {
        Some(t) => println!("Installing Attention Getter {t} (now running {})", env!("CARGO_PKG_VERSION")),
        None => println!(
            "Updating Attention Getter to the latest release (now running {})",
            env!("CARGO_PKG_VERSION")
        ),
    }
    platform::run(pin.as_deref())
}

#[cfg(not(windows))]
mod platform {
    use std::process::Command;

    use anyhow::{Context, Result, bail};

    pub fn run(pin: Option<&str>) -> Result<()> {
        let url = format!("{}/install.sh", super::SCRIPTS);
        let rt = tokio::runtime::Builder::new_current_thread().enable_all().build()?;
        let script = rt
            .block_on(async {
                let r = reqwest::Client::builder()
                    .user_agent(crate::config::user_agent())
                    .build()?
                    .get(&url)
                    .send()
                    .await?
                    .error_for_status()?;
                anyhow::Ok(r.text().await?)
            })
            .with_context(|| format!("downloading {url}"))?;
        // Run it from a file so stdin stays the terminal (setup needs it on a fresh config).
        let path = std::env::temp_dir().join(format!("attention-getter-install-{}.sh", std::process::id()));
        std::fs::write(&path, script)?;
        let mut cmd = Command::new("sh");
        cmd.arg(&path);
        if let Some(t) = pin {
            cmd.env("AG_VERSION", t);
        }
        // Replacing this binary while it runs is fine on Linux: the installer mv's a new file in.
        let status = cmd.status();
        let _ = std::fs::remove_file(&path);
        if !status.context("running sh")?.success() {
            bail!("the installer failed (see above)");
        }
        Ok(())
    }
}

#[cfg(windows)]
mod platform {
    use std::os::windows::process::CommandExt;
    use std::process::Command;

    use anyhow::{Context, Result};

    const CREATE_NEW_CONSOLE: u32 = 0x0000_0010;

    /// A running exe can't be replaced on Windows, and the installer stops every copy anyway,
    /// so the installer runs in its own PowerShell window and this process exits right away.
    pub fn run(pin: Option<&str>) -> Result<()> {
        let url = format!("{}/install.ps1", super::SCRIPTS);
        // TLS 1.2 first: Windows PowerShell 5.1 may not offer it by default, and GitHub needs it.
        let script = format!(
            "[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072; \
             try {{ irm '{url}' | iex }} catch {{ Write-Host $_ -ForegroundColor Red }}; \
             Read-Host 'Press Enter to close'"
        );
        let mut cmd = Command::new("powershell.exe");
        cmd.args(["-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script.as_str()])
            .creation_flags(CREATE_NEW_CONSOLE);
        if let Some(t) = pin {
            cmd.env("AG_VERSION", t);
        }
        cmd.spawn().context("starting PowerShell")?;
        println!("The update is running in a new PowerShell window.");
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn version_tags() {
        assert_eq!(tag("1.2.3").unwrap(), "v1.2.3");
        assert_eq!(tag("v1.10.0").unwrap(), "v1.10.0");
        for bad in ["latest", "v1.2", "1.2.3-rc1", "v1..3", ""] {
            assert!(tag(bad).is_err(), "{bad}");
        }
    }
}
