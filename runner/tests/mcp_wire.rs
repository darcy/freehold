//! Wire-shape verification (Phase A3): the runner speaks MCP over HTTP.
//! A real JSON-RPC client (ureq) does the full handshake and calls tools,
//! proving transport + registry framing — not just that the crate compiles.

use freehold_runner::mcp;
use serde_json::{json, Value};
use ureq::http::Response;
use ureq::Body;

fn post(
    agent: &ureq::Agent,
    url: &str,
    body: Value,
    session: Option<&str>,
) -> Result<Response<Body>, Box<dyn std::error::Error>> {
    let mut req = agent
        .post(url)
        .header("Content-Type", "application/json")
        .header("Accept", "application/json");
    if let Some(s) = session {
        req = req.header("mcp-session-id", s);
    }
    Ok(req.send_json(&body)?)
}

fn body_json(resp: Response<Body>) -> Value {
    resp.into_body().read_json::<Value>().expect("json body")
}

fn rpc(id: u64, method: &str, params: Value) -> Value {
    json!({ "jsonrpc": "2.0", "id": id, "method": method, "params": params })
}

// Blocking ureq client + async axum server need separate workers; a
// current-thread runtime deadlocks (client blocks the only worker).
#[tokio::test(flavor = "multi_thread")]
async fn mcp_wire_shape() {
    let (addr, server) = mcp::serve("127.0.0.1:0").await.expect("bind server");
    let url = format!("http://{addr}/mcp");
    let agent = ureq::Agent::new_with_defaults();

    // 1. initialize handshake
    let resp = post(
        &agent,
        &url,
        rpc(
            1,
            "initialize",
            json!({
                "protocolVersion": "2025-06-18",
                "capabilities": {},
                "clientInfo": { "name": "wire-test", "version": "0.0.1" }
            }),
        ),
        None,
    )
    .expect("initialize");
    let session = resp
        .headers()
        .get("mcp-session-id")
        .map(|v| v.to_str().expect("ascii session").to_string());
    assert!(session.is_some(), "server must issue a session id");
    let init = body_json(resp);
    assert_eq!(init["id"], 1);
    assert!(
        init["result"]["capabilities"]["tools"].is_object(),
        "server must advertise tools capability"
    );
    assert!(
        !init["result"]["protocolVersion"].as_str().unwrap_or("").is_empty(),
        "server must negotiate a protocol version"
    );

    // 2. initialized notification (no id -> no body)
    post(
        &agent,
        &url,
        json!({ "jsonrpc": "2.0", "method": "notifications/initialized" }),
        session.as_deref(),
    )
    .expect("initialized notification");

    // 3. tools/list — the full runner contract
    let tools = body_json(post(
        &agent,
        &url,
        rpc(2, "tools/list", json!({})),
        session.as_deref(),
    )
    .expect("tools/list"));
    let names: Vec<String> = tools["result"]["tools"]
        .as_array()
        .expect("tools array")
        .iter()
        .map(|t| t["name"].as_str().expect("tool name").to_string())
        .collect();
    assert_eq!(names, vec!["list", "exec", "config", "status", "snapshot"]);

    // 4. tools/call list — real, returns the (empty) registry
    let list = body_json(post(
        &agent,
        &url,
        rpc(3, "tools/call", json!({ "name": "list", "arguments": {} })),
        session.as_deref(),
    )
    .expect("tools/call list"));
    let text = &list["result"]["content"][0]["text"];
    let targets: Value =
        serde_json::from_str(text.as_str().expect("list text")).expect("targets json");
    assert_eq!(targets, json!([]), "Phase A registry is empty");

    // 5. tools/call config — real, non-secret server info
    let cfg = body_json(post(
        &agent,
        &url,
        rpc(4, "tools/call", json!({ "name": "config", "arguments": {} })),
        session.as_deref(),
    )
    .expect("tools/call config"));
    assert!(
        cfg["result"]["content"][0]["text"].as_str().unwrap().contains("freehold-runner"),
        "config names the runner"
    );

    // 6. pending phases answer with typed isError, not protocol errors
    for (id, tool) in [5u64, 6, 7].into_iter().zip(["exec", "status", "snapshot"]) {
        let call = body_json(post(
            &agent,
            &url,
            rpc(id, "tools/call", json!({ "name": tool, "arguments": {} })),
            session.as_deref(),
        )
        .unwrap_or_else(|e| panic!("tools/call {tool}: {e}")));
        assert_eq!(call["result"]["isError"], true, "{tool} is pending in Phase A");
    }

    // 7. unknown tool -> typed error, still MCP-compliant
    let unknown = body_json(post(
        &agent,
        &url,
        rpc(8, "tools/call", json!({ "name": "bogus", "arguments": {} })),
        session.as_deref(),
    )
    .expect("tools/call bogus"));
    assert_eq!(unknown["result"]["isError"], true);

    // 8. unknown method -> JSON-RPC protocol error
    let bad_method = body_json(post(
        &agent,
        &url,
        rpc(9, "resources/list", json!({})),
        session.as_deref(),
    )
    .expect("resources/list"));
    assert_eq!(bad_method["error"]["code"], -32601);

    server.abort();
}
