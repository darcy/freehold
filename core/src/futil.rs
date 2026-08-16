//! Atomic, permission-pinned file discipline for secret material.
//!
//! One answer to "how do I write a secret-bearing file safely": unique
//! 0600-at-birth temp + fsync + atomic rename (unix). Also the 0700 state-dir
//! helper used everywhere secret material lands.

use std::fs::{self, OpenOptions};
use std::io::{self, Write};
use std::path::{Path, PathBuf};

use rand::RngCore;

/// Ensure `dir` exists and is 0700 (unix). Only tightens a pre-existing LOOSE
/// dir — a user-supplied --state-dir pointing at a shared dir isn't silently
/// chmodded when already tight, but gets logged when it is.
pub fn ensure_private_dir(dir: &Path) -> io::Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
        let mut b = fs::DirBuilder::new();
        b.mode(0o700).recursive(true);
        b.create(dir)?;
        let mode = fs::metadata(dir)?.permissions().mode();
        if mode & 0o077 != 0 {
            tracing::warn!(
                dir = %dir.display(),
                mode = format_args!("{mode:o}"),
                "tightening dir to 0700 (holds secret material)"
            );
            fs::set_permissions(dir, fs::Permissions::from_mode(0o700))?;
        }
    }
    #[cfg(not(unix))]
    fs::create_dir_all(dir)?;
    Ok(())
}

/// Open a temp file for secret material: born 0600 (unix) via O_CREAT|O_EXCL,
/// with a UNIQUE per-invocation name (~2⁻³² collision per pair).
///
/// Two properties fall out of the random suffix:
/// - concurrent runs never share an inode, so no run can unlink or write
///   through another run's temp;
/// - a temp stranded by a killed run is never reused with its (possibly loose)
///   existing bits and never unlinked by a live run — it stays as a rare,
///   0600, 0700-dir'd leftover.
///
/// On the vanishingly rare actual name collision, `AlreadyExists` re-rolls a
/// fresh name instead of touching the existing file.
pub fn open_secret_temp(dst: &Path, name: &str) -> io::Result<(fs::File, PathBuf)> {
    for _ in 0..4 {
        let tmp = dst.with_file_name(format!("{name}.tmp.{}", temp_suffix()));
        let mut opts = OpenOptions::new();
        opts.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            opts.mode(0o600);
        }
        match opts.open(&tmp) {
            Ok(f) => return Ok((f, tmp)),
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(e),
        }
    }
    Err(io::Error::new(
        io::ErrorKind::AlreadyExists,
        "could not allocate a unique temp name",
    ))
}

fn temp_suffix() -> String {
    let mut b = [0u8; 4];
    rand::rng().fill_bytes(&mut b);
    hex::encode(b)
}

/// Write `bytes` to `path` atomically: unique 0600 temp (unix) + fsync +
/// rename. Never exposes secret material in a loose or partially-written file;
/// a failure removes the temp, never leaving a stray copy.
pub fn write_0600_atomic(path: &Path, bytes: &[u8]) -> io::Result<()> {
    let name = path
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or("secret");
    let (mut f, tmp) = open_secret_temp(path, name)?;
    let wrote = f.write_all(bytes).and_then(|_| f.sync_all());
    if wrote.is_err() {
        let _ = fs::remove_file(&tmp);
        wrote?;
    }
    if let Err(e) = fs::rename(&tmp, path) {
        let _ = fs::remove_file(&tmp);
        return Err(e);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn atomic_write_roundtrip() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("blob");
        write_0600_atomic(&path, b"secret-material").unwrap();
        assert_eq!(fs::read(&path).unwrap(), b"secret-material");
        assert_eq!(fs::read_dir(dir.path()).unwrap().count(), 1, "no temp leftovers");
    }

    #[cfg(unix)]
    #[test]
    fn atomic_write_is_0600() {
        use std::os::unix::fs::PermissionsExt;
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("blob");
        write_0600_atomic(&path, b"secret-material").unwrap();
        let mode = fs::metadata(&path).unwrap().permissions().mode();
        assert_eq!(mode & 0o077, 0, "secret file must not be group/other readable");
    }
}
