//! Local cache of each attention type's GIF and sound, kept in sync with the server.

use std::collections::HashSet;
use std::path::{Path, PathBuf};

use anyhow::{Context, Result, bail};
use serde::Deserialize;

use crate::config::Config;
use crate::protocol::{MediaPaths, TypeRef};

#[derive(Debug, Deserialize)]
struct Manifest {
    types: Vec<TypeRef>,
}

pub struct Cache {
    dir: PathBuf,
    etag: Option<String>,
}

impl Cache {
    pub fn new(dir: PathBuf) -> Self {
        Cache { dir, etag: None }
    }

    fn path(&self, t: &TypeRef, kind: &str) -> PathBuf {
        self.dir.join(format!("{}-{}.{kind}", t.id, t.hash))
    }

    /// Paths for a type if both files are cached.
    pub fn lookup(&self, t: &TypeRef) -> Option<MediaPaths> {
        let (gif, sound) = (self.path(t, "gif"), self.path(t, "sound"));
        (gif.is_file() && sound.is_file()).then_some(MediaPaths {
            gif: Some(gif),
            sound: Some(sound),
        })
    }

    /// Downloads a type's files if missing.
    pub async fn ensure(&self, http: &reqwest::Client, cfg: &Config, t: &TypeRef) -> Result<MediaPaths> {
        if let Some(p) = self.lookup(t) {
            return Ok(p);
        }
        tokio::fs::create_dir_all(&self.dir).await?;
        for kind in ["gif", "sound"] {
            let dest = self.path(t, kind);
            if dest.is_file() {
                continue;
            }
            let url = format!("{}/media/{}/{kind}?h={}", cfg.server(), t.id, t.hash);
            download(http, cfg, &url, &dest)
                .await
                .with_context(|| format!("downloading {} {kind}", t.name))?;
        }
        self.lookup(t).context("media missing after download")
    }

    /// Fetches the manifest (cheap 304 when unchanged), downloads new types and prunes stale files.
    pub async fn sync(&mut self, http: &reqwest::Client, cfg: &Config) -> Result<()> {
        let mut req = http.get(format!("{}/api/device/manifest", cfg.server())).bearer_auth(&cfg.token);
        if let Some(etag) = &self.etag {
            req = req.header(reqwest::header::IF_NONE_MATCH, etag);
        }
        let resp = req.send().await?;
        if resp.status() == reqwest::StatusCode::NOT_MODIFIED {
            return Ok(());
        }
        if !resp.status().is_success() {
            bail!("manifest: HTTP {}", resp.status());
        }
        let etag = resp
            .headers()
            .get(reqwest::header::ETAG)
            .and_then(|v| v.to_str().ok())
            .map(String::from);
        let m: Manifest = resp.json().await?;

        let mut keep = HashSet::new();
        for t in &m.types {
            if let Err(e) = self.ensure(http, cfg, t).await {
                crate::log!("media sync: {e:#}");
                continue; // retry at next sync; don't store the etag
            }
            keep.insert(self.path(t, "gif"));
            keep.insert(self.path(t, "sound"));
        }
        if keep.len() == m.types.len() * 2 {
            self.etag = etag;
        }
        if let Ok(mut rd) = tokio::fs::read_dir(&self.dir).await {
            while let Ok(Some(e)) = rd.next_entry().await {
                let p = e.path();
                if !keep.contains(&p) {
                    let _ = tokio::fs::remove_file(&p).await;
                }
            }
        }
        Ok(())
    }
}

async fn download(http: &reqwest::Client, cfg: &Config, url: &str, dest: &Path) -> Result<()> {
    let resp = http.get(url).bearer_auth(&cfg.token).send().await?;
    if !resp.status().is_success() {
        bail!("HTTP {}", resp.status());
    }
    let bytes = resp.bytes().await?;
    let tmp = dest.with_extension("part");
    tokio::fs::write(&tmp, &bytes).await?;
    tokio::fs::rename(&tmp, dest).await?;
    Ok(())
}
