//! Chunk 1 acceptance script — run it, read the report, trust the exit code.
//!
//! Everything runs hermetic on loopback (mock Vultr/B2 APIs + in-process
//! sshd). Exit 0 = every Chunk-1 acceptance criterion holds; otherwise the
//! failing checks and their details are printed before a non-zero exit.

use anyhow::Result;

#[tokio::main]
async fn main() -> Result<()> {
    println!("freehold — Chunk 1 acceptance");
    println!("（reclaim the future we were promised）\n");
    let checks = freehold_acceptance::run_checks().await;
    let mut failed = 0;
    for c in &checks {
        let mark = if c.ok { "PASS" } else { "FAIL" };
        println!("[{mark}] {} — {}", c.id, c.description);
        if let Some(d) = &c.detail {
            println!("      {d}");
        }
        if !c.ok {
            failed += 1;
        }
    }
    println!(
        "\n{}/{} acceptance checks passed",
        checks.len() - failed,
        checks.len()
    );
    if failed > 0 {
        std::process::exit(1);
    }
    Ok(())
}
