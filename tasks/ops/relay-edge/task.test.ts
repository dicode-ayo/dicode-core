import { setupHarness } from "../../sdk-test.ts";
import { assertInvariant, buildDesiredRecords, type DesiredRecord } from "./task.ts";
await setupHarness(import.meta.url);

const ZONES = "https://api.cloudflare.com/client/v4/zones?name=*";
const DNS = "https://api.cloudflare.com/client/v4/zones/zone123/dns_records*";
const DNS_URL = "https://api.cloudflare.com/client/v4/zones/zone123/dns_records";
const TUNNELS = "https://api.cloudflare.com/client/v4/accounts/*/cfd_tunnel*";

function ok(result: unknown) {
  return { status: 200, body: { success: true, result } };
}

function setCreds() {
  env.set("CLOUDFLARE_API_TOKEN", "cf-token");
  env.set("CLOUDFLARE_ACCOUNT_ID", "acct-1");
}

function setParams(over: Record<string, string> = {}) {
  params.set("zone", "dicode.io");
  params.set("public_hostname", "relay.dicode.io");
  params.set("control_hostname", "broker.relay.dicode.io");
  params.set("control_ip", "203.0.113.7");
  for (const [k, v] of Object.entries(over)) params.set(k, v);
}

test("the invariant guard throws when a control record is proxied", async () => {
  const proxiedControl: DesiredRecord[] = [
    { role: "control", type: "A", name: "broker.relay.dicode.io", content: "203.0.113.7", proxied: true, ttl: 1 },
  ];
  await assert.throws(() => assertInvariant(proxiedControl), /must never be proxied/);
  // Derived records satisfy the invariant by construction.
  assertInvariant(buildDesiredRecords({
    control_hostname: "broker.relay.dicode.io",
    control_ip: "203.0.113.7",
    public_hostname: "relay.dicode.io",
    tunnelId: "tunnel-abc",
  }));
});

test("dry_run plans a missing control record and makes only GET calls", async () => {
  setCreds();
  setParams();
  http.mock("GET", ZONES, ok([{ id: "zone123" }]));
  http.mock("GET", DNS, ok([]));

  const result = await runTask();

  assert.equal(result.dry_run, true);
  assert.equal(result.drift, true);
  assert.equal(result.changes.length, 1);
  assert.equal(result.changes[0].name, "broker.relay.dicode.io");
  assert.httpCalled("GET", DNS);
  assert.httpNotCalled("POST", DNS);
  assert.httpNotCalled("PATCH", "https://api.cloudflare.com/client/v4/zones/zone123/dns_records/*");
});

test("apply creates the control A (proxied:false) and the public CNAME (proxied:true)", async () => {
  setCreds();
  setParams({ dry_run: "false", tunnel_name: "relay-tunnel" });
  http.mock("GET", ZONES, ok([{ id: "zone123" }]));
  http.mock("GET", TUNNELS, ok([{ id: "tunnel-abc" }]));
  http.mock("GET", DNS, ok([]));
  http.mock("POST", DNS, ok({ id: "rec-new" }));

  const result = await runTask();

  assert.equal(result.dry_run, false);
  assert.equal(result.applied.length, 2);

  // Control record is created first, so httpCalledWith (first match) sees it.
  assert.httpCalledWith("POST", DNS_URL, {
    body: { type: "A", name: "broker.relay.dicode.io", content: "203.0.113.7", proxied: false, ttl: 1 },
  });
  // Public CNAME is the last POST to the same URL.
  const publicBody = http.lastRequestBody("POST", DNS_URL);
  assert.equal(publicBody, {
    type: "CNAME",
    name: "relay.dicode.io",
    content: "tunnel-abc.cfargotunnel.com",
    proxied: true,
    ttl: 1,
  });
});

test("a CF success:false envelope surfaces the errors", async () => {
  setCreds();
  setParams();
  http.mock("GET", ZONES, {
    status: 200,
    body: { success: false, errors: [{ code: 1000, message: "invalid token" }] },
  });

  await assert.throws(() => runTask(), /invalid token/);
});

test("zone lookup returning zero results throws", async () => {
  setCreds();
  setParams();
  http.mock("GET", ZONES, ok([]));

  await assert.throws(() => runTask(), /zone not found/);
});
