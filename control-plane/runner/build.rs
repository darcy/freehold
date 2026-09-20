// Emit the release version identity into the runner binary. The justfile/CI
// pass FREEHOLD_VERSION (git describe) + FREEHOLD_COMMIT; a plain `cargo build`
// falls back to the crate version, so tests never need the stamp.
fn main() {
    let version = std::env::var("FREEHOLD_VERSION")
        .ok()
        .filter(|v| !v.is_empty())
        .unwrap_or_else(|| std::env::var("CARGO_PKG_VERSION").unwrap_or_else(|_| "dev".into()));
    let commit = std::env::var("FREEHOLD_COMMIT")
        .ok()
        .filter(|v| !v.is_empty())
        .unwrap_or_else(|| "unknown".into());
    println!("cargo:rustc-env=FREEHOLD_VERSION={version}");
    println!("cargo:rustc-env=FREEHOLD_COMMIT={commit}");
    println!("cargo:rerun-if-env-changed=FREEHOLD_VERSION");
    println!("cargo:rerun-if-env-changed=FREEHOLD_COMMIT");
}
