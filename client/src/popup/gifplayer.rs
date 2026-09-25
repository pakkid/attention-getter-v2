//! Streaming GIF playback: decodes one frame at a time onto a single canvas, so memory
//! stays at one frame (plus one backup for "restore previous") however long the GIF is.

use std::fs::File;
use std::io::BufReader;
use std::path::{Path, PathBuf};
use std::time::Duration;

use anyhow::{Result, bail};
use gif::{ColorOutput, DecodeOptions, Decoder, DisposalMethod};

pub struct GifPlayer {
    path: PathBuf,
    decoder: Decoder<BufReader<File>>,
    pub width: usize,
    pub height: usize,
    canvas: Vec<u8>,
    backup: Vec<u8>,
    /// Disposal to apply before drawing the next frame: (method, left, top, w, h).
    pending: Option<(DisposalMethod, usize, usize, usize, usize)>,
}

fn open(path: &Path) -> Result<Decoder<BufReader<File>>> {
    let mut opts = DecodeOptions::new();
    opts.set_color_output(ColorOutput::RGBA);
    Ok(opts.read_info(BufReader::new(File::open(path)?))?)
}

impl GifPlayer {
    pub fn open(path: &Path) -> Result<Self> {
        let decoder = open(path)?;
        let (width, height) = (decoder.width() as usize, decoder.height() as usize);
        if width == 0 || height == 0 {
            bail!("empty GIF");
        }
        Ok(GifPlayer {
            path: path.to_path_buf(),
            decoder,
            width,
            height,
            canvas: vec![0; width * height * 4],
            backup: Vec::new(),
            pending: None,
        })
    }

    /// The current composited frame as RGBA.
    pub fn rgba(&self) -> &[u8] {
        &self.canvas
    }

    /// Advances to the next frame (looping forever) and returns how long to show it.
    pub fn advance(&mut self) -> Result<Duration> {
        self.dispose_previous();
        for attempt in 0..2 {
            match self.decoder.read_next_frame()? {
                Some(frame) => {
                    let (l, t, w, h) = (frame.left as usize, frame.top as usize, frame.width as usize, frame.height as usize);
                    if frame.dispose == DisposalMethod::Previous {
                        self.backup.clear();
                        self.backup.extend_from_slice(&self.canvas);
                    }
                    let delay = frame.delay;
                    let dispose = frame.dispose;
                    blit(&mut self.canvas, self.width, self.height, &frame.buffer, l, t, w, h);
                    self.pending = Some((dispose, l, t, w, h));
                    // Browsers treat delays under 20 ms as 100 ms; do the same.
                    let ms = if delay < 2 { 100 } else { delay as u64 * 10 };
                    return Ok(Duration::from_millis(ms));
                }
                None if attempt == 0 => {
                    // End of the animation: rewind by reopening the file.
                    self.decoder = open(&self.path)?;
                    self.canvas.fill(0);
                    self.pending = None;
                }
                None => break,
            }
        }
        bail!("GIF has no frames")
    }

    fn dispose_previous(&mut self) {
        match self.pending.take() {
            Some((DisposalMethod::Background, l, t, w, h)) => {
                for y in t..(t + h).min(self.height) {
                    let row = y * self.width;
                    let (a, b) = ((row + l).min(row + self.width), (row + l + w).min(row + self.width));
                    self.canvas[a * 4..b * 4].fill(0);
                }
            }
            Some((DisposalMethod::Previous, ..)) if self.backup.len() == self.canvas.len() => {
                self.canvas.copy_from_slice(&self.backup);
            }
            _ => {}
        }
    }
}

/// Draws a frame's RGBA pixels onto the canvas; fully transparent pixels leave the canvas as is.
#[allow(clippy::too_many_arguments)]
fn blit(canvas: &mut [u8], cw: usize, ch: usize, src: &[u8], l: usize, t: usize, w: usize, h: usize) {
    for y in 0..h {
        let cy = t + y;
        if cy >= ch {
            break;
        }
        for x in 0..w {
            let cx = l + x;
            if cx >= cw {
                break;
            }
            let s = (y * w + x) * 4;
            if s + 3 >= src.len() {
                return;
            }
            if src[s + 3] != 0 {
                let d = (cy * cw + cx) * 4;
                canvas[d..d + 4].copy_from_slice(&src[s..s + 4]);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn blit_respects_transparency_and_bounds() {
        let mut canvas = vec![9u8; 3 * 2 * 4];
        // 2x2 frame at (2,1): only (2,1) lands on the 3x2 canvas; second pixel is transparent.
        let src = [1, 1, 1, 255, 0, 0, 0, 0, 2, 2, 2, 255, 3, 3, 3, 255];
        blit(&mut canvas, 3, 2, &src, 2, 1, 2, 2);
        assert_eq!(&canvas[(3 + 2) * 4..(3 + 2) * 4 + 4], &[1, 1, 1, 255]);
        assert_eq!(&canvas[0..4], &[9, 9, 9, 9]);
    }
}
