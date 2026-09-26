//! Background service: holds the WebSocket to the server, syncs media and runs popups.
//!
//! Kept deliberately small: a single-threaded tokio runtime, no GUI libraries touched.
//! Each alert spawns this same binary as `popup`, so window/GL/audio memory is released
//! as soon as the popup closes.

use std::process::Stdio;
use std::time::Duration;

use anyhow::{Context, Result};
use futures_util::{SinkExt, StreamExt};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStdin, Command};
use tokio::sync::mpsc;
use tokio_tungstenite::tungstenite::{self, Message, client::IntoClientRequest};

use crate::config::{self, Config};
use crate::media::Cache;
use crate::protocol::{Alert, DeviceMsg, MediaPaths, PopupIn, PopupOut, ServerMsg};

const SYNC_EVERY: Duration = Duration::from_secs(5 * 60);
/// The server pings every 30 s; silence longer than this means the connection is dead.
const READ_TIMEOUT: Duration = Duration::from_secs(75);

struct Popup {
    alert_id: i64,
    child: Child,
    stdin: ChildStdin,
}

enum Event {
    PopupOut(i64, PopupOut),
    PopupExited(i64),
    MediaReady(i64, MediaPaths),
}

struct Daemon {
    cfg: Config,
    http: reqwest::Client,
    cache: Cache,
    popup: Option<Popup>,
    /// Replies not yet delivered to the server (sent after reconnect).
    outbox: Vec<DeviceMsg>,
    /// Alerts answered locally; ignore re-deliveries until the server catches up.
    answered: Vec<i64>,
    events_tx: mpsc::UnboundedSender<Event>,
}

pub fn run() -> Result<()> {
    let cfg = Config::load()?;
    let rt = tokio::runtime::Builder::new_current_thread().enable_all().build()?;
    rt.block_on(async move {
        let (events_tx, mut events_rx) = mpsc::unbounded_channel();
        let http = reqwest::Client::builder()
            .timeout(Duration::from_secs(60))
            .user_agent(config::user_agent())
            .build()?;
        let mut d = Daemon {
            cache: Cache::new(config::cache_dir()?),
            cfg,
            http,
            popup: None,
            outbox: vec![],
            answered: vec![],
            events_tx,
        };
        crate::log!("starting; server {}, pc {}", d.cfg.server(), d.cfg.name);
        if let Err(e) = d.cache.sync(&d.http, &d.cfg).await {
            crate::log!("initial media sync failed: {e:#}");
        }

        let mut backoff = Duration::from_secs(1);
        loop {
            match d.session(&mut events_rx).await {
                Ok(()) => backoff = Duration::from_secs(1),
                Err(e) => crate::log!("connection: {e:#}"),
            }
            // Keep handling popup output while waiting to reconnect.
            let wait = tokio::time::sleep(backoff);
            tokio::pin!(wait);
            loop {
                tokio::select! {
                    _ = &mut wait => break,
                    Some(ev) = events_rx.recv() => d.handle_event(ev, None).await,
                }
            }
            backoff = (backoff * 2).min(Duration::from_secs(60));
        }
    })
}

type Ws = tokio_tungstenite::WebSocketStream<tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>>;

impl Daemon {
    async fn connect(&self) -> Result<Ws> {
        let mut req = self.cfg.ws_url().into_client_request()?;
        req.headers_mut()
            .insert("Authorization", format!("Bearer {}", self.cfg.token).parse()?);
        req.headers_mut().insert("User-Agent", config::user_agent().parse()?);
        let (ws, _) = tokio::time::timeout(Duration::from_secs(20), tokio_tungstenite::connect_async(req))
            .await
            .context("connect timeout")?
            .map_err(|e| match e {
                tungstenite::Error::Http(r) if r.status() == 401 => {
                    anyhow::anyhow!("device token rejected; run `attention-getter setup` again")
                }
                e => e.into(),
            })?;
        Ok(ws)
    }

    /// One connected session. Returns Ok when the server closed cleanly.
    async fn session(&mut self, events_rx: &mut mpsc::UnboundedReceiver<Event>) -> Result<()> {
        let mut ws = self.connect().await?;
        crate::log!("connected");
        for m in std::mem::take(&mut self.outbox) {
            ws.send(Message::text(serde_json::to_string(&m)?)).await?;
        }
        let mut sync = tokio::time::interval(SYNC_EVERY);
        sync.tick().await; // first tick is immediate; we synced at startup
        loop {
            tokio::select! {
                msg = tokio::time::timeout(READ_TIMEOUT, ws.next()) => {
                    let msg = match msg {
                        Err(_) => anyhow::bail!("no ping from server in {READ_TIMEOUT:?}"),
                        Ok(None) => return Ok(()),
                        Ok(Some(m)) => m?,
                    };
                    match msg {
                        Message::Text(t) => match serde_json::from_str::<ServerMsg>(&t) {
                            Ok(m) => self.handle_server(m, &mut ws).await?,
                            Err(e) => crate::log!("bad server message: {e}"),
                        },
                        Message::Close(_) => return Ok(()),
                        _ => {} // pings are answered by tungstenite
                    }
                }
                Some(ev) = events_rx.recv() => self.handle_event(ev, Some(&mut ws)).await,
                _ = sync.tick() => self.sync().await,
            }
        }
    }

    async fn sync(&mut self) {
        if let Err(e) = self.cache.sync(&self.http, &self.cfg).await {
            crate::log!("media sync failed: {e:#}");
        }
    }

    async fn handle_server(&mut self, m: ServerMsg, ws: &mut Ws) -> Result<()> {
        match m {
            ServerMsg::Alert { alert } => {
                if self.answered.contains(&alert.id) {
                    return Ok(());
                }
                let id = alert.id;
                self.show(alert).await;
                ws.send(Message::text(serde_json::to_string(&DeviceMsg::Ack { alert_id: id })?))
                    .await?;
            }
            ServerMsg::Cancel { alert_id } => {
                if let Some(mut p) = self.popup.take_if(|p| p.alert_id == alert_id) {
                    let _ = p.child.start_kill();
                }
            }
            ServerMsg::ManifestChanged => self.sync().await,
        }
        Ok(())
    }

    /// Shows an alert immediately; media that isn't cached yet is fetched in the background.
    async fn show(&mut self, alert: Alert) {
        if let Some(p) = self.popup.as_mut() {
            if p.alert_id == alert.id {
                send_popup(&mut p.stdin, &PopupIn::Update { alert }).await;
                return;
            }
            // A different alert replaced the one on screen (e.g. it was answered elsewhere).
            let _ = p.child.start_kill();
            self.popup = None;
        }

        let media = alert.r#type.as_ref().and_then(|t| self.cache.lookup(t)).unwrap_or_default();
        if media.gif.is_none()
            && let Some(t) = alert.r#type.clone()
        {
            // Not cached: pull it now and hand it to the popup when it lands.
            let (http, cfg, tx, id) = (self.http.clone(), self.cfg.clone(), self.events_tx.clone(), alert.id);
            let cache = Cache::new(config::cache_dir().unwrap_or_default());
            tokio::spawn(async move {
                match cache.ensure(&http, &cfg, &t).await {
                    Ok(m) => {
                        let _ = tx.send(Event::MediaReady(id, m));
                    }
                    Err(e) => crate::log!("fetching media for {}: {e:#}", t.name),
                }
            });
        }

        let init = PopupIn::Init {
            alert: alert.clone(),
            media,
            monitor: self.cfg.monitor.clone(),
            hotkey: self.cfg.hotkey.clone(),
        };
        match spawn_popup(alert.id, &self.events_tx).await {
            Ok(mut p) => {
                send_popup(&mut p.stdin, &init).await;
                self.popup = Some(p);
            }
            Err(e) => crate::log!("could not start popup: {e:#}"),
        }
    }

    async fn handle_event(&mut self, ev: Event, ws: Option<&mut Ws>) {
        match ev {
            Event::PopupOut(id, out) => {
                let msg = match out {
                    PopupOut::Reply { text } => DeviceMsg::Reply { alert_id: id, text },
                    PopupOut::Dismiss => DeviceMsg::Dismiss { alert_id: id },
                };
                self.answered.push(id);
                if self.answered.len() > 32 {
                    self.answered.remove(0);
                }
                let sent = match ws {
                    Some(ws) => ws.send(Message::text(serde_json::to_string(&msg).unwrap())).await.is_ok(),
                    None => false,
                };
                if !sent {
                    self.outbox.push(msg);
                }
            }
            Event::PopupExited(id) => {
                if self.popup.as_ref().is_some_and(|p| p.alert_id == id) {
                    self.popup = None;
                }
            }
            Event::MediaReady(id, media) => {
                if let Some(p) = self.popup.as_mut().filter(|p| p.alert_id == id) {
                    send_popup(&mut p.stdin, &PopupIn::Media { media }).await;
                }
            }
        }
    }
}

async fn spawn_popup(alert_id: i64, events: &mpsc::UnboundedSender<Event>) -> Result<Popup> {
    let exe = std::env::current_exe()?;
    let mut child = Command::new(exe)
        .arg("popup")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .kill_on_drop(true)
        .spawn()?;
    let stdin = child.stdin.take().context("popup stdin")?;
    let stdout = child.stdout.take().context("popup stdout")?;
    let tx = events.clone();
    tokio::spawn(async move {
        let mut lines = BufReader::new(stdout).lines();
        while let Ok(Some(line)) = lines.next_line().await {
            match serde_json::from_str::<PopupOut>(&line) {
                Ok(out) => {
                    let _ = tx.send(Event::PopupOut(alert_id, out));
                }
                Err(e) => crate::log!("bad popup output {line:?}: {e}"),
            }
        }
        let _ = tx.send(Event::PopupExited(alert_id));
    });
    Ok(Popup { alert_id, child, stdin })
}

async fn send_popup(stdin: &mut ChildStdin, m: &PopupIn) {
    let mut line = serde_json::to_string(m).unwrap();
    line.push('\n');
    if let Err(e) = stdin.write_all(line.as_bytes()).await {
        crate::log!("popup stdin: {e}");
    }
    let _ = stdin.flush().await;
}
