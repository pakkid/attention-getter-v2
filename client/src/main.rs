// No console window on Windows; CLI subcommands re-attach to the parent console.
#![cfg_attr(windows, windows_subsystem = "windows")]

mod config;
mod daemon;
mod install;
mod media;
mod popup;
mod protocol;
mod setup;
mod update;

use anyhow::Result;

#[macro_export]
macro_rules! log {
    ($($t:tt)*) => { eprintln!("[attention-getter] {}", format!($($t)*)) };
}

const USAGE: &str = "attention-getter: shows a fullscreen attention popup when triggered from your phone or Alexa.

USAGE:
    attention-getter setup        Pair this PC with the server and choose monitor + hotkey
    attention-getter run          Run the background service (what autostart runs);
                                  on Windows add --console to see its log in the terminal
    attention-getter install      Start automatically at login (and install the GNOME extension)
    attention-getter uninstall    Remove autostart
    attention-getter update [vX.Y.Z]
                                  Download and install the latest release (or the given
                                  version); pairing and settings are kept
    attention-getter test [GIF] [SOUND]
                                  Show a local test popup
    attention-getter monitors     List monitor names";

fn main() {
    #[cfg(windows)]
    let interactive = windows_console();
    let res = real_main();
    if let Err(e) = &res {
        log!("error: {e:#}");
    }
    #[cfg(windows)]
    if interactive {
        // Our own console window closes on exit; let the user read it first.
        println!("\nPress Enter to close.");
        let _ = std::io::stdin().read_line(&mut String::new());
    }
    if res.is_err() {
        std::process::exit(1);
    }
}

fn real_main() -> Result<()> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match args.first().map(String::as_str).unwrap_or("run") {
        "run" => daemon::run(),
        "popup" => popup::run_from_stdin(),
        "setup" => setup::run(),
        "install" => install::install(),
        "uninstall" => install::uninstall(),
        "update" => update::run(args.get(1).map(String::as_str)),
        "monitors" => {
            for m in setup::monitors()? {
                println!("{m}");
            }
            Ok(())
        }
        "test" => {
            let gif = args.get(1).map(Into::into);
            let sound = args.get(2).map(Into::into);
            let cfg = config::Config::load().ok();
            let init = protocol::PopupIn::Init {
                alert: protocol::Alert {
                    id: 0,
                    r#type: Some(protocol::TypeRef {
                        id: 0,
                        name: "test".into(),
                        hash: String::new(),
                    }),
                    requests: vec![protocol::Request {
                        name: "You".into(),
                        message: "This is a test".into(),
                    }],
                    presets: vec!["Coming now".into(), "5 min".into(), "15 min".into(), "Busy, later".into()],
                },
                media: protocol::MediaPaths { gif, sound },
                monitor: cfg.as_ref().map(|c| c.monitor.clone()).unwrap_or_default(),
                hotkey: cfg.map(|c| c.hotkey).unwrap_or_else(config::default_hotkey),
            };
            popup::run(init, false)
        }
        "-h" | "--help" | "help" => {
            println!("{USAGE}");
            Ok(())
        }
        other => anyhow::bail!("unknown command {other:?}\n\n{USAGE}"),
    }
}

/// The binary uses the GUI subsystem so the service and popup never show a console.
/// CLI output re-attaches to the calling shell's console; interactive `setup` gets its own
/// console window, because a GUI-subsystem process would fight the shell for keyboard input.
/// The service (`run`, also the default) and popups never attach: a process attached to a
/// console is killed when that window closes. To watch the service's log, run
/// `attention-getter run --console`.
/// Returns whether a console window was opened.
#[cfg(windows)]
fn windows_console() -> bool {
    use windows_sys::Win32::System::Console::{ATTACH_PARENT_PROCESS, AllocConsole, AttachConsole};
    let args: Vec<String> = std::env::args().skip(1).collect();
    let cmd = args.first().map_or("run", String::as_str);
    let interactive = cmd == "setup";
    let attach = match cmd {
        "run" => args.iter().any(|a| a == "--console"),
        "popup" => false,
        _ => true,
    };
    // SAFETY: plain Win32 calls; they fail harmlessly when not applicable.
    unsafe {
        if interactive {
            AllocConsole();
        } else if attach {
            AttachConsole(ATTACH_PARENT_PROCESS);
        }
    }
    interactive
}
