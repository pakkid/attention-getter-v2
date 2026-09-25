// Companion extension for attention-getter on GNOME (Wayland clients can neither refuse
// focus when mapped nor focus themselves on a global key, so the shell does both).
import Meta from 'gi://Meta';
import Shell from 'gi://Shell';
import GLib from 'gi://GLib';
import * as Main from 'resource:///org/gnome/shell/ui/main.js';
import {Extension} from 'resource:///org/gnome/shell/extensions/extension.js';

const WM_CLASS = 'attention-getter-popup';
// Focus given to a new popup within this window is handed back to the previous window.
const GUARD_MS = 3000;

const isPopup = (w) => w?.get_wm_class?.() === WM_CLASS;
const alive = (w) => w && w.get_compositor_private() !== null;

export default class AttentionGetterExtension extends Extension {
    enable() {
        this._settings = this.getSettings();
        this._lastFocus = null;
        this._guardUntil = 0;

        Main.wm.addKeybinding('focus-key', this._settings, Meta.KeyBindingFlags.IGNORE_AUTOREPEAT,
            Shell.ActionMode.NORMAL | Shell.ActionMode.OVERVIEW | Shell.ActionMode.POPUP,
            () => this._focusPopup());

        this._focusId = global.display.connect('notify::focus-window', () => this._onFocusChanged());
        this._createdId = global.display.connect('window-created', (_d, win) => this._onCreated(win));
    }

    disable() {
        Main.wm.removeKeybinding('focus-key');
        global.display.disconnect(this._focusId);
        global.display.disconnect(this._createdId);
        this._settings = null;
        this._lastFocus = null;
    }

    _onCreated(win) {
        if (!isPopup(win))
            return;
        this._guardUntil = GLib.get_monotonic_time() / 1000 + GUARD_MS;
        win.make_above();
        win.stick(); // follow the user across workspaces
        // The popup is a plain monitor-sized window (not fullscreen, which other extensions
        // react to, e.g. by moving it to a new workspace); place it once it is mapped.
        this._place(win);
        const id = win.connect('shown', () => {
            win.disconnect(id);
            this._place(win);
        });
    }

    // Moves the popup onto the monitor named in its title ("Attention Getter [DP-3]").
    _place(win) {
        const m = /\[([^\]]+)\]$/.exec(win.get_title() ?? '');
        const idx = m ? global.backend.get_monitor_manager().get_monitor_for_connector(m[1]) : -1;
        if (idx < 0)
            return;
        if (win.get_monitor() !== idx)
            win.move_to_monitor(idx);
        const g = global.display.get_monitor_geometry(idx);
        win.move_resize_frame(false, g.x, g.y, g.width, g.height);
    }

    _onFocusChanged() {
        const w = global.display.focus_window;
        if (!w)
            return;
        if (!isPopup(w)) {
            this._lastFocus = w;
            return;
        }
        if (GLib.get_monotonic_time() / 1000 > this._guardUntil)
            return; // user focused it (hotkey or click)
        this._guardUntil = 0;
        if (alive(this._lastFocus)) {
            // focus() doesn't raise, so the popup stays on top of a fullscreen app.
            this._lastFocus.focus(global.get_current_time());
            w.raise();
        }
    }

    _focusPopup() {
        const win = global.get_window_actors().map((a) => a.meta_window).find(isPopup);
        if (!win)
            return;
        this._guardUntil = 0;
        Main.activateWindow(win);
    }
}
