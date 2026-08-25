//! TUI modes (bootstrap / configure / running) + shared stage machinery.

pub mod bootstrap;
pub mod configure;
pub mod running;

use anyhow::Result;
use std::sync::mpsc::{Receiver, Sender, channel};

/// A sequential stage runner: one stage at a time, on a worker thread, with
/// progress + result shipped back over a channel the render loop drains.
pub struct StageRunner {
    pub tx: Sender<RawMsg>,
    pub rx: Receiver<RawMsg>,
}

/// The stage/check protocol (sequential ⇒ a Done belongs to the current job):
///   - ok: "1" / an ok-message / protocol payloads below
///   - provision → "pub:<ssh-ed25519 …>" or "pub:" (reused runner)
///   - serve     → "<pid>"
///   - verify    → "ok" | "auth:<tail…>" | "fail:<tail…>"
#[derive(Debug, Clone)]
pub enum RawMsg {
    Done { ok: bool, out: String },
}

impl StageRunner {
    pub fn new() -> Self {
        let (tx, rx) = channel();
        Self { tx, rx }
    }

    /// Spawn a stage; the callback returns the human-facing ok payload.
    pub fn spawn<F>(&self, f: F)
    where
        F: FnOnce() -> Result<String> + Send + 'static,
    {
        let tx = self.tx.clone();
        std::thread::spawn(move || match f() {
            Ok(out) => {
                let _ = tx.send(RawMsg::Done { ok: true, out });
            }
            Err(e) => {
                let _ = tx.send(RawMsg::Done {
                    ok: false,
                    out: format!("{e:#}"),
                });
            }
        });
    }
}

impl Default for StageRunner {
    fn default() -> Self {
        Self::new()
    }
}
