//! In-process russh SSH server for the C1 connector tests and the Phase G
//! acceptance script — a real ssh daemon (kex, host keys, exec channel)
//! with no external dependency. Exec commands run via `sh -c` and their
//! stdout is returned over the channel, exactly like a real box would.

use std::net::SocketAddr;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use rand::RngCore;
use russh::keys::ssh_key::private::Ed25519Keypair;
use russh::keys::ssh_key::{LineEnding, PrivateKey, PublicKey};
use russh::server::{self, Auth, ChannelOpenHandle, Handler as ServerHandler, Msg as ServerMsg};
use russh::{Channel, ChannelId};
use tokio::net::TcpListener;

pub fn random_ed25519() -> PrivateKey {
    // ssh-key pins its own rand_core (older than rand 0.9); build the signing
    // key from our own randomness via ed25519-dalek and convert up.
    let mut b = [0u8; 32];
    rand::rng().fill_bytes(&mut b);
    let signing = ed25519_dalek::SigningKey::from_bytes(&b);
    PrivateKey::from(Ed25519Keypair::from(signing))
}

/// A client keypair + its OpenSSH PEM encoding (what a CP-shipped ssh
/// credential looks like).
pub fn client_key_pem() -> (PrivateKey, String) {
    let key = random_ed25519();
    let pem = key.to_openssh(LineEnding::LF).unwrap();
    (key, pem.to_string())
}

#[derive(Clone, Default)]
pub struct ServerState {
    pub connections: Arc<AtomicUsize>,
    pub reject_all_keys: bool,
}

#[derive(Clone)]
struct TestServer {
    state: ServerState,
}

impl server::Server for TestServer {
    type Handler = Self;
    fn new_client(&mut self, _: Option<SocketAddr>) -> Self {
        (*self).clone()
    }
}

impl ServerHandler for TestServer {
    type Error = Box<dyn std::error::Error + Send + Sync>;

    async fn auth_publickey(&mut self, _: &str, _: &PublicKey) -> Result<Auth, Self::Error> {
        // One auth == one new connection (the pooling metric).
        self.state.connections.fetch_add(1, Ordering::SeqCst);
        if self.state.reject_all_keys {
            Ok(Auth::Reject {
                proceed_with_methods: None,
                partial_success: false,
            })
        } else {
            Ok(Auth::Accept)
        }
    }

    async fn channel_open_session(
        &mut self,
        _channel: Channel<ServerMsg>,
        reply: ChannelOpenHandle,
        _session: &mut server::Session,
    ) -> Result<(), Self::Error> {
        reply.accept().await;
        Ok(())
    }

    async fn exec_request(
        &mut self,
        channel: ChannelId,
        data: &[u8],
        session: &mut server::Session,
    ) -> Result<(), Self::Error> {
        let cmd = String::from_utf8_lossy(data);
        // async child: a blocking Command::output() on a worker thread would
        // stall the runtime (and the client's timeout timers).
        let output = tokio::process::Command::new("sh")
            .arg("-c")
            .arg(cmd.as_ref())
            .output()
            .await?;
        session
            .handle()
            .data(channel, output.stdout)
            .await
            .map_err(|b| std::io::Error::other(format!("ssh send data: {b:?}")))?;
        session
            .handle()
            .exit_status_request(channel, output.status.code().unwrap_or(1) as u32)
            .await
            .map_err(|()| std::io::Error::other("ssh send exit status"))?;
        session
            .handle()
            .close(channel)
            .await
            .map_err(|()| std::io::Error::other("ssh send close"))?;
        Ok(())
    }
}

/// Boot an in-process sshd on an ephemeral loopback port; returns the
/// address and the connection counter. The accept-loop task dies with the
/// test runtime.
pub async fn spawn_server(reject_all_keys: bool) -> (SocketAddr, Arc<ServerState>) {
    let state = Arc::new(ServerState {
        connections: Arc::new(AtomicUsize::new(0)),
        reject_all_keys,
    });
    let mut config = server::Config::default();
    config.keys.push(random_ed25519());
    let config = Arc::new(config);
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let server_state = state.clone();
    tokio::spawn(async move {
        loop {
            let (socket, _) = match listener.accept().await {
                Ok(x) => x,
                Err(_) => break,
            };
            let handler = TestServer {
                state: (*server_state).clone(),
            };
            let config = config.clone();
            tokio::spawn(async move {
                let _ = server::run_stream(config, socket, handler).await;
            });
        }
    });
    (addr, state)
}
