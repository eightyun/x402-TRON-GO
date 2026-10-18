import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import net from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, test } from "node:test";
import { fileURLToPath } from "node:url";

// 仅此测试进程禁网；不为上游 SDK 提供 approval/广播能力。
let networkAttempts = 0;
const originalConnect = net.Socket.prototype.connect;
const originalFetch = globalThis.fetch;
const denyNetwork = () => {
  networkAttempts++;
  throw new Error("离线互操作测试禁止网络访问");
};
net.Socket.prototype.connect = denyNetwork;
globalThis.fetch = denyNetwork;

const { TronWeb, utils } = await import("tronweb");
const { ExactTronScheme: UpstreamClient } = await import("@bankofai/x402-tron/exact/client");
const { ExactTronScheme: UpstreamFacilitator } = await import("@bankofai/x402-tron/exact/facilitator");
const { TRON_NILE, TRON_SHASTA, PERMIT2_ADDRESSES, X402_PERMIT2_PROXY_ADDRESSES } = await import("@bankofai/x402-tron");
const { x402Client } = await import("@bankofai/x402-core/client");
const { x402Facilitator } = await import("@bankofai/x402-core/facilitator");

const repository = fileURLToPath(new URL("../../", import.meta.url));
const temporary = mkdtempSync(join(tmpdir(), "x402-sdk-interop-"));
const binary = join(temporary, "bridge");
after(() => {
  net.Socket.prototype.connect = originalConnect;
  globalThis.fetch = originalFetch;
  // 仅清理此测试刚创建的临时目录，不使用调用方提供的路径。
  rmSync(temporary, { recursive: true, force: true });
  assert.ok(networkAttempts === 0, "测试发生了非预期网络访问");
});
const built = spawnSync("go", ["build", "-o", binary, "./interop/bankofai/bridge"], {
  cwd: repository,
  env: { ...process.env, GOPROXY: "off", GOSUMDB: "off", GOTOOLCHAIN: "local", GOWORK: "off" },
  timeout: 60_000,
  maxBuffer: 64 << 10,
});
assert.ok(built.status === 0, "Go互操作工具构建失败；先准备Go模块缓存，不允许测试自动下载");

function bridge(operation, config, requirements, payload = undefined, maxAmount = requirements.amount) {
  const result = spawnSync(binary, [], {
    input: JSON.stringify({ operation, config, requirements, payload, maxAmount }),
    encoding: "utf8",
    timeout: 15_000,
    maxBuffer: 64 << 10,
  });
  // 不将子进程完整输出交给断言，避免失败日志暴露临时可广播授权。
  assert.ok(result.status === 0, "Go互操作工具拒绝请求或执行失败");
  try {
    return JSON.parse(result.stdout);
  } catch {
    throw new Error("Go互操作工具未返回有效JSON");
  }
}

function normalize(address) {
  const value = address.startsWith("0x") ? "41" + address.slice(2) : address;
  return TronWeb.address.toHex(value).toLowerCase();
}

function configuration(network, upfront) {
  // 使用上游包自身的配置作为互操作域，不将这些常量视为已核验链上部署。
  const config = { network, permit2Address: PERMIT2_ADDRESSES[network], proxyAddress: X402_PERMIT2_PROXY_ADDRESSES[network] };
  assert.ok(typeof config.permit2Address === "string" && typeof config.proxyAddress === "string", "上游缺少测试网合约配置");
  const requirements = {
    scheme: "exact", network, asset: "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf", amount: "1000",
    payTo: "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL", maxTimeoutSeconds: 60,
    extra: { assetTransferMethod: "permit2", ...(upfront ? { paymentFlow: "upfront" } : {}) },
  };
  return { config, requirements };
}

async function upstreamPayload(requirements) {
  const key = randomBytes(32);
  const privateKey = key.toString("hex");
  const address = TronWeb.address.fromPrivateKey(privateKey);
  assert.ok(typeof address === "string", "临时付款人生成失败");
  const tronweb = new TronWeb({ fullHost: "https://offline.invalid" });
  let signs = 0;
  const signer = {
    address,
    signTypedData: async ({ domain, types, primaryType, message }) => {
      assert.ok(primaryType === "PermitWitnessTransferFrom", "上游未采用Permit2见证授权");
      signs++;
      return tronweb.trx._signTypedData(domain, types, message, privateKey);
    },
    readContract: async () => { throw new Error("离线客户端不应查询或修改账户状态"); },
  };
  try {
    const client = new x402Client().register(requirements.network, new UpstreamClient(signer));
    client.setSpendControls({ allowedAssets: [{ network: requirements.network, asset: requirements.asset, maxAmountPerPayment: requirements.amount }] });
    const payload = await client.createPaymentPayload({ x402Version: 2, accepts: [requirements] });
    assert.ok(signs === 1 && payload.x402Version === 2 && payload.accepted !== undefined, "上游core未产生完整v2授权");
    return { payload, payer: address };
  } finally {
    key.fill(0);
  }
}

function upstreamVerifier(config, requirements, payer) {
  const statistics = { signatures: 0, reads: 0, writes: 0 };
  const signer = {
    getAddresses: () => [requirements.payTo],
    verifyTypedData: async ({ address, domain, types, primaryType, message, signature }) => {
      statistics.signatures++;
      assert.ok(primaryType === "PermitWitnessTransferFrom", "上游验签未使用Permit2见证授权");
      try {
        const recovered = utils.typedData.verifyTypedData(domain, types, message, signature);
        return normalize(recovered) === normalize(address);
      } catch {
        return false;
      }
    },
    readContract: async ({ address, functionName, args }) => {
      statistics.reads++;
      assert.ok(normalize(address) === normalize(requirements.asset), "上游查询了非预期资产");
      assert.ok(normalize(args[0]) === normalize(payer), "上游查询了非预期付款人");
      if (functionName === "allowance") {
        assert.ok(args.length === 2 && normalize(args[1]) === normalize(config.permit2Address), "上游查询了错误授权对象");
      } else {
        assert.ok(functionName === "balanceOf" && args.length === 1, "上游发起了非预期只读调用");
      }
      // 账户状态明确为模拟；验签仍使用上游构造的类型/域和真正的TronWeb密码学。
      return BigInt(requirements.amount);
    },
    writeContract: async () => { statistics.writes++; throw new Error("离线测试禁止广播"); },
    waitForTransactionReceipt: async () => { throw new Error("离线测试没有链上回执"); },
  };
  const core = new x402Facilitator().register(config.network, new UpstreamFacilitator(signer));
  return {
    statistics,
    verify: async (payload, quote = requirements) => {
      try {
        const result = await core.verify(payload, quote);
        return result.isValid === true;
      } catch {
        return false;
      }
    },
  };
}

test("固定上游实际发布包版本", () => {
  for (const [path, version] of [
    ["@bankofai/x402-tron", "1.2.0"], ["@bankofai/x402-core", "1.1.1"], ["tronweb", "6.3.0"],
  ]) {
    const metadata = JSON.parse(readFileSync(new URL(`node_modules/${path}/package.json`, import.meta.url), "utf8"));
    assert.ok(metadata.version === version, "安装版本偏离互操作验证基线");
  }
});

for (const network of [TRON_NILE, TRON_SHASTA]) {
  for (const upfront of [false, true]) {
    const name = `${network} ${upfront ? "upfront" : "legacy-extra"}`;
    test(`${name}: 上游core/client → Go verifier`, async () => {
      const { config, requirements } = configuration(network, upfront);
      const { payload, payer } = await upstreamPayload(requirements);
      const verified = bridge("verify", config, requirements, payload);
      assert.ok(verified.valid && normalize(verified.payer) === normalize(payer), "Go拒绝真实上游SDK生成的授权");
    });

    test(`${name}: Go client → 上游core/facilitator`, async () => {
      const { config, requirements } = configuration(network, upfront);
      const generated = bridge("create", config, requirements);
      const verifier = upstreamVerifier(config, requirements, generated.payer);
      assert.ok(await verifier.verify(generated.payload), "上游SDK拒绝Go公开客户端生成的授权");
      assert.ok(verifier.statistics.signatures === 1 && verifier.statistics.reads === 2 && verifier.statistics.writes === 0, "未经过真实密码学核验或账户查询越界");
    });
  }
}

test("双向拒绝授权和报价篡改", async (t) => {
  const { config, requirements } = configuration(TRON_NILE, true);
  const upstream = await upstreamPayload(requirements);
  const generated = bridge("create", config, requirements);
  const mutations = [
    ["signature", (payload) => { payload.payload.signature = "0x" + "0".repeat(130); }],
    ["amount", (payload) => { payload.payload.permit2Authorization.permitted.amount = "1001"; }],
    ["recipient", (payload) => { payload.payload.permit2Authorization.witness.to = "0x" + normalize(config.proxyAddress).slice(2); }],
    ["spender", (payload) => { payload.payload.permit2Authorization.spender = "0x" + normalize(requirements.asset).slice(2); }],
    ["nonce", (payload) => { payload.payload.permit2Authorization.nonce = "0x" + (BigInt(payload.payload.permit2Authorization.nonce) ^ 1n).toString(16).padStart(64, "0"); }],
    ["deadline", (payload) => { payload.payload.permit2Authorization.deadline = "1"; }],
    ["validAfter", (payload) => { payload.payload.permit2Authorization.witness.validAfter = String(Math.floor(Date.now() / 1000) + 3600); }],
    ["network", (_payload, quote) => { quote.network = TRON_SHASTA; }],
  ];
  for (const [name, mutation] of mutations) {
    await t.test(name, async () => {
      const incoming = structuredClone(upstream.payload);
      const incomingQuote = structuredClone(requirements);
      mutation(incoming, incomingQuote);
      assert.ok(!bridge("verify", config, incomingQuote, incoming).valid, "Go放行篡改的上游授权");
      const outgoing = structuredClone(generated.payload);
      const outgoingQuote = structuredClone(requirements);
      mutation(outgoing, outgoingQuote);
      const verifier = upstreamVerifier(config, requirements, generated.payer);
      assert.ok(!(await verifier.verify(outgoing, outgoingQuote)), "上游放行篡改的Go授权");
      assert.ok(verifier.statistics.writes === 0, "验证期间触发广播");
    });
  }
});

test("Go可信域和完整accepted绑定不接受上游数据覆盖", async () => {
  const { config, requirements } = configuration(TRON_NILE, true);
  const { payload } = await upstreamPayload(requirements);
  const wrongDomain = { ...config, permit2Address: requirements.asset };
  assert.ok(!bridge("verify", wrongDomain, requirements, payload).valid, "Go接受错误Permit2签名域");
  const stripped = structuredClone(payload);
  delete stripped.accepted.extra.paymentFlow;
  assert.ok(!bridge("verify", config, requirements, stripped).valid, "Go接受不一致的upfront报价");
  const differentQuote = { ...requirements, payTo: config.proxyAddress };
  assert.ok(!bridge("verify", config, differentQuote, payload).valid, "Go接受替换的可信收款报价");
});

test("上游1.2.0不支持十进制网络报价：明确保留兼容边界", async () => {
  const { config, requirements } = configuration(TRON_NILE, true);
  const canonical = "tron:" + BigInt(TRON_NILE.split(":")[1]).toString(10);
  const quote = { ...requirements, network: canonical };
  assert.ok(PERMIT2_ADDRESSES[canonical] === undefined, "上游能力发生变化，需要复核兼容性声明");
  await assert.rejects(() => upstreamPayload(quote), /No Permit2 contract address configured/);
  const generated = bridge("create", { ...config, network: canonical }, quote);
  assert.ok(generated.valid, "Go规范十进制报价创建失败");
  const upstream = upstreamVerifier(config, requirements, generated.payer);
  assert.ok(!(await upstream.verify(generated.payload, quote)), "上游实际接受十进制报价，应重新核对测试基线");
});
