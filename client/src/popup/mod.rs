//! The fullscreen popup: GIF + looping sound + reply box. Runs as its own short-lived
//! process (`attention-getter popup`), driven by JSON lines on stdin, replying on stdout.

mod gifplayer;
mod platform;

use std::io::{BufRead, Write};
use std::num::NonZeroU32;
use std::path::Path;
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{Context, Result};
use egui::{Color32, FontId, Key, Rect, RichText, Vec2, pos2};
use glutin::context::{NotCurrentGlContext, PossiblyCurrentContext};
use glutin::display::{GetGlDisplay, GlDisplay};
use glutin::surface::{GlSurface, Surface, WindowSurface};
use raw_window_handle::HasWindowHandle;
use winit::application::ApplicationHandler;
use winit::event::{StartCause, WindowEvent};
use winit::event_loop::{ActiveEventLoop, ControlFlow, EventLoop, EventLoopProxy};
use winit::monitor::MonitorHandle;
use winit::window::{Fullscreen, Window, WindowAttributes, WindowId, WindowLevel};

use crate::protocol::{Alert, MediaPaths, PopupIn, PopupOut};
use gifplayer::GifPlayer;

/// App id / WM_CLASS; the GNOME extension matches windows by this.
#[cfg_attr(not(target_os = "linux"), allow(dead_code))]
pub const WINDOW_CLASS: &str = "attention-getter-popup";

#[derive(Debug)]
enum UserEvent {
    Input(PopupIn),
    InputClosed,
    Hotkey,
    Repaint(Duration),
}

/// Entry point for `attention-getter popup`: the first stdin line is the Init message.
pub fn run_from_stdin() -> Result<()> {
    let mut first = String::new();
    std::io::stdin().lock().read_line(&mut first)?;
    let init: PopupIn = serde_json::from_str(&first).context("popup init")?;
    run(init, true)
}

/// Runs the popup. With `follow_stdin`, further stdin lines update it and EOF closes it.
pub fn run(init: PopupIn, follow_stdin: bool) -> Result<()> {
    let PopupIn::Init {
        alert,
        media,
        monitor,
        hotkey,
    } = init
    else {
        anyhow::bail!("first popup message must be init");
    };
    let event_loop = EventLoop::<UserEvent>::with_user_event().build()?;
    let proxy = event_loop.create_proxy();
    if follow_stdin {
        let p = proxy.clone();
        std::thread::spawn(move || {
            for line in std::io::stdin().lock().lines() {
                let Ok(line) = line else { break };
                match serde_json::from_str(&line) {
                    Ok(m) => {
                        let _ = p.send_event(UserEvent::Input(m));
                    }
                    Err(e) => crate::log!("popup: bad input: {e}"),
                }
            }
            let _ = p.send_event(UserEvent::InputClosed);
        });
    }
    let mut app = App::new(proxy, alert, media, monitor, hotkey);
    event_loop.run_app(&mut app)?;
    Ok(())
}

fn emit(out: &PopupOut) {
    let mut stdout = std::io::stdout().lock();
    let _ = writeln!(stdout, "{}", serde_json::to_string(out).unwrap());
    let _ = stdout.flush();
}

// Field order is drop order: GL resources must go before the window they render to.
struct Gfx {
    egui: egui_glow::EguiGlow,
    gl: Arc<glow::Context>,
    surface: Surface<WindowSurface>,
    context: PossiblyCurrentContext,
    window: Window,
}

struct Audio {
    _sink: rodio::MixerDeviceSink,
    _player: rodio::Player,
}

fn start_audio(path: &Path) -> Result<Audio> {
    let mut sink = rodio::DeviceSinkBuilder::open_default_sink()?;
    sink.log_on_drop(false);
    let player = rodio::Player::connect_new(sink.mixer());
    let file = std::io::BufReader::new(std::fs::File::open(path)?);
    player.append(rodio::Decoder::new_looped(file)?);
    Ok(Audio {
        _sink: sink,
        _player: player,
    })
}

struct App {
    proxy: EventLoopProxy<UserEvent>,
    alert: Alert,
    media: MediaPaths,
    monitor: String,
    hotkey: String,
    wayland: bool,

    gfx: Option<Gfx>,
    shown: bool,
    gif: Option<GifPlayer>,
    texture: Option<egui::TextureHandle>,
    next_frame: Option<Instant>,
    egui_repaint_at: Option<Instant>,
    audio: Option<Audio>,
    _hotkeys: Option<global_hotkey::GlobalHotKeyManager>,

    focused: bool,
    focus_text: bool,
    cursor_to_end: bool,
    text: String,
    done: bool,
}

impl App {
    fn new(proxy: EventLoopProxy<UserEvent>, alert: Alert, media: MediaPaths, monitor: String, hotkey: String) -> Self {
        App {
            proxy,
            alert,
            media: MediaPaths::default(),
            monitor,
            hotkey,
            wayland: false,
            gfx: None,
            shown: false,
            gif: None,
            texture: None,
            next_frame: None,
            egui_repaint_at: None,
            audio: None,
            _hotkeys: None,
            focused: false,
            focus_text: false,
            cursor_to_end: false,
            text: String::new(),
            done: false,
        }
        .with_media(media)
    }

    fn with_media(mut self, media: MediaPaths) -> Self {
        self.set_media(media);
        self
    }

    /// Loads media not already playing. Called at start and when a late download lands.
    fn set_media(&mut self, media: MediaPaths) {
        if self.gif.is_none()
            && let Some(p) = &media.gif
        {
            match GifPlayer::open(p) {
                Ok(g) => {
                    self.gif = Some(g);
                    self.next_frame = Some(Instant::now());
                }
                Err(e) => crate::log!("popup: gif {}: {e:#}", p.display()),
            }
        }
        if self.audio.is_none()
            && let Some(p) = &media.sound
        {
            match start_audio(p) {
                Ok(a) => self.audio = Some(a),
                Err(e) => crate::log!("popup: sound {}: {e:#}", p.display()),
            }
        }
        self.media = media;
    }

    fn pick_monitor(&self, el: &ActiveEventLoop) -> Option<MonitorHandle> {
        let wanted = self.monitor.trim();
        if !wanted.is_empty() {
            if let Some(m) = el.available_monitors().find(|m| m.name().as_deref() == Some(wanted)) {
                return Some(m);
            }
            crate::log!("popup: monitor {wanted:?} not found; using primary");
        }
        el.primary_monitor().or_else(|| el.available_monitors().next())
    }

    fn create_window(&mut self, el: &ActiveEventLoop) -> Result<()> {
        #[cfg(target_os = "linux")]
        {
            use winit::platform::wayland::ActiveEventLoopExtWayland;
            self.wayland = el.is_wayland();
        }
        let monitor = self.pick_monitor(el);
        let mut attrs = WindowAttributes::default()
            .with_title("Attention Getter")
            .with_decorations(false)
            .with_visible(false)
            .with_active(false)
            .with_window_level(WindowLevel::AlwaysOnTop);

        #[cfg(target_os = "linux")]
        {
            use winit::platform::wayland::WindowAttributesExtWayland;
            use winit::platform::x11::WindowAttributesExtX11;
            attrs = if self.wayland {
                WindowAttributesExtWayland::with_name(attrs, WINDOW_CLASS, WINDOW_CLASS)
            } else {
                WindowAttributesExtX11::with_name(attrs, WINDOW_CLASS, WINDOW_CLASS)
            };
        }
        #[cfg(windows)]
        {
            use winit::platform::windows::WindowAttributesExtWindows;
            attrs = attrs.with_skip_taskbar(true);
        }

        // Windows: a borderless window covering the monitor avoids exclusive-fullscreen
        // side effects (activation, mode switches).
        // GNOME Wayland: also a monitor-sized plain window, which the shell extension moves onto
        // the monitor named in the title. Real fullscreen trips extensions that react to it
        // (e.g. Kiwi moves fullscreen windows to a new workspace and switches to it).
        // Elsewhere: real fullscreen on the chosen output.
        let gnome = self.wayland && std::env::var("XDG_CURRENT_DESKTOP").is_ok_and(|d| d.to_ascii_uppercase().contains("GNOME"));
        if cfg!(windows) {
            if let Some(m) = &monitor {
                attrs = attrs.with_position(m.position()).with_inner_size(m.size());
            }
        } else if gnome {
            if let Some(m) = &monitor {
                attrs = attrs
                    .with_title(format!("Attention Getter [{}]", m.name().unwrap_or_default()))
                    .with_inner_size(m.size().to_logical::<f64>(m.scale_factor()));
            }
        } else {
            attrs = attrs.with_fullscreen(Some(Fullscreen::Borderless(monitor.clone())));
        }

        let template = glutin::config::ConfigTemplateBuilder::new()
            .with_depth_size(0)
            .with_stencil_size(0)
            .with_transparency(false);
        let (window, gl_config) = glutin_winit::DisplayBuilder::new()
            .with_preference(glutin_winit::ApiPreference::FallbackEgl)
            .with_window_attributes(Some(attrs.clone()))
            .build(el, template, |mut configs| configs.next().expect("no GL config"))
            .map_err(|e| anyhow::anyhow!("GL config: {e}"))?;
        let window = match window {
            Some(w) => w,
            None => glutin_winit::finalize_window(el, attrs, &gl_config)?,
        };
        let display = gl_config.display();
        let raw = window.window_handle()?.as_raw();
        let ctx_attrs = glutin::context::ContextAttributesBuilder::new().build(Some(raw));
        let gles_attrs = glutin::context::ContextAttributesBuilder::new()
            .with_context_api(glutin::context::ContextApi::Gles(None))
            .build(Some(raw));
        // SAFETY: the window handle is valid for the lifetime of `window`, which outlives the context.
        let not_current = unsafe {
            display
                .create_context(&gl_config, &ctx_attrs)
                .or_else(|_| display.create_context(&gl_config, &gles_attrs))?
        };
        let size = window.inner_size();
        let surface_attrs = glutin::surface::SurfaceAttributesBuilder::<WindowSurface>::new().build(
            raw,
            NonZeroU32::new(size.width).unwrap_or(NonZeroU32::MIN),
            NonZeroU32::new(size.height).unwrap_or(NonZeroU32::MIN),
        );
        // SAFETY: as above.
        let surface = unsafe { display.create_window_surface(&gl_config, &surface_attrs)? };
        let context = not_current.make_current(&surface)?;
        let _ = surface.set_swap_interval(&context, glutin::surface::SwapInterval::Wait(NonZeroU32::MIN));
        // SAFETY: the context is current on this thread.
        let gl = Arc::new(unsafe {
            glow::Context::from_loader_function(|s| {
                let s = std::ffi::CString::new(s).unwrap();
                display.get_proc_address(&s)
            })
        });

        let egui = egui_glow::EguiGlow::new(el, gl.clone(), None, None, true);
        let proxy = egui::mutex::Mutex::new(self.proxy.clone());
        egui.egui_ctx.set_request_repaint_callback(move |info| {
            let _ = proxy.lock().send_event(UserEvent::Repaint(info.delay));
        });
        style(&egui.egui_ctx);

        platform::before_show(&window, self.wayland);
        self.gfx = Some(Gfx {
            window,
            surface,
            context,
            egui,
            gl,
        });
        self.register_hotkey();
        Ok(())
    }

    /// Windows/X11: grab the focus hotkey while the popup is open. On GNOME Wayland the
    /// shell extension owns the binding instead.
    fn register_hotkey(&mut self) {
        if self.wayland {
            return;
        }
        let hk: global_hotkey::hotkey::HotKey = match self.hotkey.parse() {
            Ok(h) => h,
            Err(e) => {
                crate::log!("popup: invalid hotkey {:?}: {e}", self.hotkey);
                return;
            }
        };
        match global_hotkey::GlobalHotKeyManager::new() {
            Ok(m) => {
                if let Err(e) = m.register(hk) {
                    crate::log!("popup: could not register hotkey {:?}: {e}", self.hotkey);
                }
                let proxy = std::sync::Mutex::new(self.proxy.clone());
                global_hotkey::GlobalHotKeyEvent::set_event_handler(Some(move |ev: global_hotkey::GlobalHotKeyEvent| {
                    if ev.state == global_hotkey::HotKeyState::Pressed {
                        let _ = proxy.lock().unwrap().send_event(UserEvent::Hotkey);
                    }
                }));
                self._hotkeys = Some(m);
            }
            Err(e) => crate::log!("popup: hotkey manager: {e}"),
        }
    }

    fn finish(&mut self, el: &ActiveEventLoop, out: PopupOut) {
        if !self.done {
            self.done = true;
            emit(&out);
        }
        el.exit();
    }

    fn redraw(&mut self, el: &ActiveEventLoop) {
        let now = Instant::now();
        if let (Some(g), Some(t)) = (self.gif.as_mut(), self.next_frame)
            && now >= t
        {
            match g.advance() {
                Ok(delay) => {
                    let img = egui::ColorImage::from_rgba_unmultiplied([g.width, g.height], g.rgba());
                    let ctx = &self.gfx.as_ref().unwrap().egui.egui_ctx;
                    match &mut self.texture {
                        Some(tex) => tex.set(img, egui::TextureOptions::LINEAR),
                        None => self.texture = Some(ctx.load_texture("gif", img, egui::TextureOptions::LINEAR)),
                    }
                    // Don't try to catch up after a stall; just show the next frame on time.
                    self.next_frame = Some(now + delay);
                }
                Err(e) => {
                    crate::log!("popup: gif playback stopped: {e:#}");
                    self.next_frame = None;
                }
            }
        }

        let mut action = None;
        let gfx = self.gfx.as_mut().unwrap();
        let ui_state = UiState {
            alert: &self.alert,
            texture: self.texture.as_ref(),
            focused: self.focused,
            hotkey: &self.hotkey,
        };
        let text = &mut self.text;
        let focus_text = &mut self.focus_text;
        let cursor_to_end = &mut self.cursor_to_end;
        gfx.egui.run(&gfx.window, |ui| {
            action = draw(ui, &ui_state, text, focus_text, cursor_to_end);
        });
        // SAFETY: GL context is current on this thread.
        unsafe {
            use glow::HasContext;
            gfx.gl.clear_color(0.0, 0.0, 0.0, 1.0);
            gfx.gl.clear(glow::COLOR_BUFFER_BIT);
        }
        gfx.egui.paint(&gfx.window);
        let _ = gfx.surface.swap_buffers(&gfx.context);
        if !self.shown {
            gfx.window.set_visible(true);
            self.shown = true;
        }
        if let Some(out) = action {
            self.finish(el, out);
        }
    }
}

struct UiState<'a> {
    alert: &'a Alert,
    texture: Option<&'a egui::TextureHandle>,
    focused: bool,
    hotkey: &'a str,
}

fn style(ctx: &egui::Context) {
    ctx.set_visuals(egui::Visuals::dark());
    ctx.all_styles_mut(|s| {
        s.visuals.extreme_bg_color = Color32::from_rgb(28, 28, 32);
        s.visuals.selection.bg_fill = Color32::from_rgb(255, 90, 54);
        s.spacing.button_padding = Vec2::new(14.0, 8.0);
    });
}

const ACCENT: Color32 = Color32::from_rgb(255, 90, 54);

fn draw(ui: &mut egui::Ui, st: &UiState, text: &mut String, focus_text: &mut bool, cursor_to_end: &mut bool) -> Option<PopupOut> {
    let screen = ui.max_rect();
    let painter = ui.painter().clone();
    painter.rect_filled(
        screen,
        0.0,
        if st.texture.is_some() {
            Color32::BLACK
        } else {
            Color32::from_rgb(90, 18, 10)
        },
    );

    if let Some(tex) = st.texture {
        let size = tex.size_vec2();
        let scale = (screen.width() / size.x).min(screen.height() / size.y);
        let rect = Rect::from_center_size(screen.center(), size * scale);
        painter.image(tex.id(), rect, Rect::from_min_max(pos2(0.0, 0.0), pos2(1.0, 1.0)), Color32::WHITE);
    }

    // Bottom panel with who is asking and the reply box.
    let h = (screen.height() * 0.34).clamp(260.0, 520.0);
    let panel = Rect::from_min_max(pos2(screen.left(), screen.bottom() - h), screen.max);
    painter.rect_filled(panel, 0.0, Color32::from_black_alpha(215));

    let mut out = None;
    let width = (screen.width() * 0.7).min(1400.0);
    let inner = Rect::from_center_size(panel.center(), Vec2::new(width, h - 48.0));
    ui.scope_builder(egui::UiBuilder::new().max_rect(inner), |ui| {
        ui.vertical_centered(|ui| {
            let title = st
                .alert
                .r#type
                .as_ref()
                .map(|t| t.name.clone())
                .unwrap_or_else(|| "Attention!".into());
            ui.label(RichText::new(title.to_uppercase()).size(54.0).strong().color(Color32::WHITE));
            ui.add_space(6.0);
            for r in &st.alert.requests {
                let line = if r.message.is_empty() {
                    r.name.clone()
                } else {
                    format!("{}: “{}”", r.name, r.message)
                };
                ui.label(RichText::new(line).size(30.0).color(Color32::from_gray(225)));
            }
            ui.add_space(18.0);

            if !st.focused {
                ui.label(RichText::new(format!("Press {} to respond", st.hotkey)).size(28.0).color(ACCENT));
                return;
            }

            let presets = &st.alert.presets;
            // Digits 1-9 on an empty box fill in the matching quick reply.
            if text.is_empty() {
                let mut chosen = None;
                ui.input_mut(|i| {
                    i.events.retain(|e| match e {
                        egui::Event::Text(t) => match t.parse::<usize>() {
                            Ok(n) if (1..=presets.len()).contains(&n) && chosen.is_none() => {
                                chosen = Some(n - 1);
                                false
                            }
                            _ => true,
                        },
                        _ => true,
                    })
                });
                if let Some(i) = chosen {
                    *text = presets[i].clone();
                    *cursor_to_end = true;
                }
            }

            let edit = egui::TextEdit::singleline(text)
                .font(FontId::proportional(32.0))
                .hint_text(RichText::new("Why can't you come / how long?").size(32.0))
                .desired_width(width * 0.85)
                .margin(Vec2::new(14.0, 10.0))
                .char_limit(300);
            let mut res = edit.show(ui);
            if *focus_text {
                res.response.request_focus();
                *focus_text = false;
            }
            if *cursor_to_end {
                let end = egui::text::CCursor::new(text.chars().count());
                res.state.cursor.set_char_range(Some(egui::text::CCursorRange::one(end)));
                res.state.store(ui.ctx(), res.response.id);
                *cursor_to_end = false;
            }
            let enter = ui.input(|i| i.key_pressed(Key::Enter));
            if enter && !text.trim().is_empty() {
                out = Some(PopupOut::Reply {
                    text: text.trim().to_string(),
                });
            } else if enter {
                res.response.request_focus();
            }

            ui.add_space(14.0);
            ui.horizontal_wrapped(|ui| {
                let total: f32 = presets.iter().map(|p| p.len() as f32 * 11.0 + 70.0).sum();
                ui.add_space(((ui.available_width() - total) / 2.0).max(0.0));
                for (i, p) in presets.iter().enumerate() {
                    if ui.button(RichText::new(format!("{}  {p}", i + 1)).size(20.0)).clicked() {
                        out = Some(PopupOut::Reply { text: p.clone() });
                    }
                }
            });
            ui.add_space(10.0);
            ui.label(
                RichText::new("Enter: send   ·   1-9: quick reply   ·   Esc: dismiss without reply")
                    .size(17.0)
                    .color(Color32::from_gray(150)),
            );
        });
    });

    if st.focused && ui.input(|i| i.key_pressed(Key::Escape)) {
        out = Some(PopupOut::Dismiss);
    }
    out
}

impl ApplicationHandler<UserEvent> for App {
    fn resumed(&mut self, el: &ActiveEventLoop) {
        if self.gfx.is_some() {
            return;
        }
        if let Err(e) = self.create_window(el) {
            crate::log!("popup: {e:#}");
            el.exit();
            return;
        }
        self.gfx.as_ref().unwrap().window.request_redraw();
    }

    fn window_event(&mut self, el: &ActiveEventLoop, _id: WindowId, event: WindowEvent) {
        let Some(gfx) = self.gfx.as_mut() else { return };
        match &event {
            WindowEvent::RedrawRequested => {
                self.redraw(el);
                return;
            }
            WindowEvent::CloseRequested => {
                self.finish(el, PopupOut::Dismiss);
                return;
            }
            WindowEvent::Resized(size) => {
                gfx.surface.resize(
                    &gfx.context,
                    NonZeroU32::new(size.width).unwrap_or(NonZeroU32::MIN),
                    NonZeroU32::new(size.height).unwrap_or(NonZeroU32::MIN),
                );
            }
            WindowEvent::Focused(f) => {
                self.focused = *f;
                if *f {
                    self.focus_text = true;
                }
                gfx.window.request_redraw();
            }
            _ => {}
        }
        let resp = gfx.egui.on_window_event(&gfx.window, &event);
        if resp.repaint {
            gfx.window.request_redraw();
        }
    }

    fn user_event(&mut self, el: &ActiveEventLoop, ev: UserEvent) {
        match ev {
            UserEvent::Input(PopupIn::Update { alert }) => self.alert = alert,
            UserEvent::Input(PopupIn::Media { media }) => self.set_media(media),
            UserEvent::Input(PopupIn::Init { .. }) => {}
            UserEvent::InputClosed => {
                // Daemon went away (or the alert was cancelled): nothing to reply to.
                self.done = true;
                el.exit();
                return;
            }
            UserEvent::Hotkey => {
                if let Some(g) = &self.gfx {
                    platform::activate(&g.window, self.wayland);
                }
            }
            UserEvent::Repaint(delay) => {
                if delay.is_zero() {
                    self.egui_repaint_at = None;
                    if let Some(g) = &self.gfx {
                        g.window.request_redraw();
                    }
                } else {
                    self.egui_repaint_at = Instant::now().checked_add(delay);
                }
                return;
            }
        }
        if let Some(g) = &self.gfx {
            g.window.request_redraw();
        }
    }

    fn new_events(&mut self, _el: &ActiveEventLoop, cause: StartCause) {
        if let StartCause::ResumeTimeReached { .. } = cause {
            let now = Instant::now();
            let due = |t: Option<Instant>| t.is_some_and(|t| t <= now);
            if due(self.next_frame) || due(self.egui_repaint_at) {
                if due(self.egui_repaint_at) {
                    self.egui_repaint_at = None;
                }
                if let Some(g) = &self.gfx {
                    g.window.request_redraw();
                }
            }
        }
    }

    fn about_to_wait(&mut self, el: &ActiveEventLoop) {
        // Sleep until the next GIF frame or egui animation; no busy loop.
        let next = [self.next_frame, self.egui_repaint_at].into_iter().flatten().min();
        el.set_control_flow(match next {
            Some(t) => ControlFlow::WaitUntil(t),
            None => ControlFlow::Wait,
        });
    }

    fn exiting(&mut self, _el: &ActiveEventLoop) {
        if let Some(g) = self.gfx.as_mut() {
            g.egui.destroy();
        }
    }
}
