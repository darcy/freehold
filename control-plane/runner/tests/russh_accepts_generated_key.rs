//! The runner's SSH client (russh) must accept the key freehold generates —
//! proves the whole onboarding path end-to-end at the consumer boundary.
#[test]
fn russh_decodes_freehold_generated_key() {
    let (privk, _pub) = freehold_core::identity::generate_ssh_keypair("russh-test").unwrap();
    let _key = russh::keys::decode_secret_key(std::str::from_utf8(&privk).unwrap(), None)
        .expect("russh must decode the generated openssh-key-v1 PEM");
}
