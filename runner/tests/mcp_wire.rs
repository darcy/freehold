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
    origin: Option<&str>,
) -> Result<Response<Body>, Box<dyn std::error::Error>> {
    let mut req = agent
        .post(url)
        .header("Content-Type", "application/json")
        .header("Accept", "application/json");
    if let Some(s) = session {
        req = req.header("mcp-session-id", s);
    }
    if let Some(o) = origin {
        req = req.header("Origin", o);
    }
    Ok(req.send_json(&body)?)
}

fn body_json(resp: Response<Body>) -> Value {
    resp.into_body().read_json::<Value>().expect("json body")
}

fn rpc(id: u64, method: &str, params: Value) -> Value {
    json!({ "jsonrpc": "2.0", "id": id, "method": method, "params": params })
}

#[tokio::test(flavor = "multi_thread")]
async fn mcp_wire_shape() {
    let (addr, server) = mcp::serve("127.0.0.1:0").await.expect("bind server");
    let url = format!("http://{addr}/mcp");
    // 4xx (the 403 origin rejection) is a valid transport response in this test.
    let agent = ureq::Agent::new_with_config(
        ureq::config::Config::builder().http_status_as_error(false).build(),
    );

    // 1. initialize handshake (no origin -> allowed, non-browser client)
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

    // 2. initialize with a STALE session header must NOT echo it back —
    //    the client has to learn the freshly issued id.
    let resp2 = post(
        &agent,
        &url,
        rpc(
            10,
            "initialize",
            json!({
                "protocolVersion": "2025-06-18",
                "capabilities": {},
                "clientInfo": { "name": "wire-test", "version": "0.0.1" }
            }),
        ),
        Some("stale-session-header"),
        None,
    )
    .expect("initialize with stale header");
    let fresh = resp2
        .headers()
        .get("mcp-session-id")
        .map(|v| v.to_str().expect("ascii session").to_string())
        .expect("fresh session id");
    assert_ne!(fresh, "stale-session-header", "stale header must not clobber the new id");

    // 3. initialized notification (no id -> no body)
    post(
        &agent,
        &url,
        json!({ "jsonrpc": "2.0", "method": "notifications/initialized" }),
        session.as_deref(),
        None,
    )
    .expect("initialized notification");

    // 4. tools/list — the full runner contract
    let tools = body_json(post(
        &agent,
        &url,
        rpc(2, "tools/list", json!({})),
        session.as_deref(),
        None,
    )
    .expect("tools/list"));
    let names: Vec<String> = tools["result"]["tools"]
        .as_array()
        .expect("tools array")
        .iter()
        .map(|t| t["name"].as_str().expect("tool name").to_string())
        .collect();
    assert_eq!(names, vec!["list", "exec", "config", "status", "snapshot"]);

    // 5. tools/call list — real, returns the (empty) registry
    let list = body_json(post(
        &agent,
        &url,
        rpc(3, "tools/call", json!({ "name": "list", "arguments": {} })),
        session.as_deref(),
        None,
    )
    .expect("tools/call list"));
    let text = &list["result"]["content"][0]["text"];
    let targets: Value =
        serde_json::from_str(text.as_str().expect("list text")).expect("targets json");
    assert_eq!(targets, json!([]), "Phase A registry is empty");

    // 6. tools/call config — real, non-secret server info
    let cfg = body_json(post(
        &agent,
        &url,
        rpc(4, "tools/call", json!({ "name": "config", "arguments": {} })),
        session.as_deref(),
        None,
    )
    .expect("tools/call config"));
    assert!(
        cfg["result"]["content"][0]["text"].as_str().unwrap().contains("freehold-runner"),
        "config names the runner"
    );

    // 7. pending phases answer with typed isError, not protocol errors
    for (id, tool) in [5u64, 6, 7].into_iter().zip(["exec", "status", "snapshot"]) {
        let call = body_json(post(
            &agent,
            &url,
            rpc(id, "tools/call", json!({ "name": tool, "arguments": {} })),
            session.as_deref(),
            None,
        )
        .unwrap_or_else(|e| panic!("tools/call {tool}: {e}")));
        assert_eq!(call["result"]["isError"], true, "{tool} is pending in Phase A");
    }

    // 8. unknown tool -> typed error, still MCP-compliant
    let unknown = body_json(post(
        &agent,
        &url,
        rpc(8, "tools/call", json!({ "name": "bogus", "arguments": {} })),
        session.as_deref(),
        None,
    )
    .expect("tools/call bogus"));
    assert_eq!(unknown["result"]["isError"], true);

    // 9. unknown method -> JSON-RPC protocol error
    let bad_method = body_json(post(
        &agent,
        &url,
        rpc(9, "resources/list", json!({})),
        session.as_deref(),
        None,
    )
    .expect("resources/list"));
    assert_eq!(bad_method["error"]["code"], -32601);

    // 10. notification with an unknown method must NOT get a response (RFC §4.1)
    let no_resp = post(
        &agent,
        &url,
        json!({ "jsonrpc": "2.0", "method": "bogus/notification" }),
        session.as_deref(),
        None,
    )
    .expect("bogus notification");
    assert_eq!(no_resp.status(), 204);

    // 11. DNS-rebinding guard: non-loopback Origin -> 403
    let blocked = post(
        &agent,
        &url,
        rpc(20, "tools/list", json!({})),
        session.as_deref(),
        Some("http://evil.example"),
    )
    .expect("evil origin");
    assert_eq!(blocked.status(), 403, "non-loopback origin must be rejected");

    // 12. loopback Origin (any port, e.g. a local UI) is allowed
    let allowed = post(
        &agent,
        &url,
        rpc(21, "tools/list", json!({})),
        session.as_deref(),
        Some("http://localhost:5173"),
    )
    .expect("loopback origin");
    assert_eq!(allowed.status(), 200);

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn refuses_non_loopback_bind() {
    let err = mcp::serve("0.0.0.0:0").await.expect_err("must refuse non-loopback bind");
    assert!(
        err.to_string().contains("non-loopback"),
        "unexpected error: {err}"
    );
}
