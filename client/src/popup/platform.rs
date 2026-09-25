//! Per-platform window tweaks: showing without stealing focus, and focusing on the hotkey.
//!
//! - Windows: `with_active(false)` (SW_SHOWNOACTIVATE) at creation; winit's `focus_window`
//!   on hotkey (the hotkey press grants the foreground right).
//! - X11: `_NET_WM_USER_TIME = 0` before mapping tells the WM not to focus the window;
//!   `_NET_ACTIVE_WINDOW` with source "pager" focuses it on hotkey.
//! - GNOME Wayland: clients can't do either, so the bundled GNOME Shell extension
//!   keeps focus where it was and binds the hotkey (see `gnome-extension/`).

use winit::window::Window;

#[cfg(target_os = "linux")]
mod x11 {
    use raw_window_handle::{HasWindowHandle, RawWindowHandle};
    use winit::window::Window;
    use x11rb::connection::Connection;
    use x11rb::protocol::xproto::{AtomEnum, ClientMessageEvent, ConnectionExt, EventMask, PropMode};
    use x11rb::wrapper::ConnectionExt as _;

    fn xid(w: &Window) -> Option<u32> {
        match w.window_handle().ok()?.as_raw() {
            RawWindowHandle::Xlib(h) => Some(h.window as u32),
            RawWindowHandle::Xcb(h) => Some(h.window.get()),
            _ => None,
        }
    }

    pub fn no_focus_on_map(w: &Window) -> anyhow::Result<()> {
        let Some(win) = xid(w) else { return Ok(()) };
        let (conn, _) = x11rb::connect(None)?;
        let atom = conn.intern_atom(false, b"_NET_WM_USER_TIME")?.reply()?.atom;
        conn.change_property32(PropMode::REPLACE, win, atom, AtomEnum::CARDINAL, &[0])?;
        conn.flush()?;
        Ok(())
    }

    pub fn activate(w: &Window) -> anyhow::Result<()> {
        let Some(win) = xid(w) else { return Ok(()) };
        let (conn, screen) = x11rb::connect(None)?;
        let root = conn.setup().roots[screen].root;
        let atom = conn.intern_atom(false, b"_NET_ACTIVE_WINDOW")?.reply()?.atom;
        // data: source indication 2 (pager: user-requested), timestamp 0 (CurrentTime).
        let ev = ClientMessageEvent::new(32, win, atom, [2u32, 0, 0, 0, 0]);
        conn.send_event(false, root, EventMask::SUBSTRUCTURE_REDIRECT | EventMask::SUBSTRUCTURE_NOTIFY, ev)?;
        conn.flush()?;
        Ok(())
    }
}

/// Called after the window exists but before it is made visible.
pub fn before_show(_w: &Window, _wayland: bool) {
    #[cfg(target_os = "linux")]
    if !_wayland && let Err(e) = x11::no_focus_on_map(_w) {
        crate::log!("x11 user time: {e:#}");
    }
}

/// Focuses the popup in response to the hotkey.
pub fn activate(w: &Window, _wayland: bool) {
    #[cfg(target_os = "linux")]
    if !_wayland && let Err(e) = x11::activate(w) {
        crate::log!("x11 activate: {e:#}");
    }
    w.focus_window();
}
