# x402-tron-go

[![CI](https://github.com/eightyun/x402-tron-go/actions/workflows/ci.yml/badge.svg)](https://github.com/eightyun/x402-tron-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/eightyun/x402-tron-go.svg)](https://pkg.go.dev/github.com/eightyun/x402-tron-go)
[![Release](https://img.shields.io/github/v/release/eightyun/x402-tron-go?include_prereleases&sort=semver)](https://github.com/eightyun/x402-tron-go/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

[English](README.md) | 简体中文

**为 [x402](https://x402.org) 提供 TRON 支持的 Go SDK。** x402-tron-go 把 TRON 网络接入官方 [x402 Go SDK](https://github.com/x402-foundation/x402/tree/main/go)，让客户端、API 服务端和 facilitator 无需自行实现 TRON 协议细节，即可通过 HTTP 402 收付 TRC-20 代币。

> [!WARNING]
> **测试网候选版本。** 本项目未经独立安全审计，请勿用于真实资金。本项目不隶属于 x402 Foundation 或 TRON。

## 特性

- **官方 SDK 机制**：覆盖 x402 的三种角色，分别实现 `SchemeNetworkClient`、`SchemeNetworkServer` 和 `SchemeNetworkFacilitator`，采用 `exact` 方案、Permit2 授权和 `upfront` 付款流程。
- **是一个库，不是平台**：导入到自己的应用即可使用，不需要托管网关、Docker 镜像或额外部署服务。
- **可选的持久化 facilitator**：基于 PostgreSQL 的结算日志保证每个授权最多只广播一次；崩溃、超时或重启后，恢复时复用原签名交易，不会重复付款。
- **到账确认后才交付**：构建或广播交易前，facilitator 会先校验签名、合约代码、余额、授权额度和 nonce，并完整模拟结算调用。只有交易固化、核验到精确的代币转账后，才报告成功。
- **默认拒绝不可信输入**：严格解码，拒绝未知、重复和有歧义的字段；节点响应有大小上限，并与固定的创世区块核对；错误信息经过脱敏。
- **可与官方 TypeScript SDK 互通**：已与已发布的 [`@bankofai/x402-tron`](https://www.npmjs.com/package/@bankofai/x402-tron) 在 Nile 测试网上完成双向实付验证。

## 状态

| 角色 | 包 | 状态 |
| --- | --- | --- |
| 客户端 / Agent | `exact` | 可用于测试网 |
| 资源服务端 | `exact` | 可用于测试网，需要可信的 facilitator |
| Facilitator | `exact/postgres` | 可用于测试网，已验证 Nile 实付与崩溃恢复 |

| 网络 | CAIP-2 标识 | 状态 |
| --- | --- | --- |
| Nile 测试网 | `tron:3448148188` | 已验证实付 |
| Shasta 测试网 | `tron:2494104990` | 仅离线测试 |
| 主网 | `tron:728126428` | 未验证，示例程序会拒绝 |

当前版本为 `v0.1.0-rc.*`，在 `v0.1.0` 之前公共 API 仍可能调整。

## 安装

需要 **Go 1.26.10 或更高版本**，这是官方 x402 Go SDK 的最低要求。建议使用最新的 Go 1.26 补丁版，以获得标准库的安全修复。

```bash
go get github.com/eightyun/x402-tron-go/exact@v0.1.0-rc.3

# 仅自建持久化 facilitator 时需要
go get github.com/eightyun/x402-tron-go/exact/postgres@v0.1.0-rc.3
```

| 包 | 用途 | 依赖 |
| --- | --- | --- |
| `github.com/eightyun/x402-tron-go` | TRON 地址与网络工具 | 仅标准库 |
| `.../exact` | 客户端与资源服务端机制、离线验签、链上预检、结算交易构建与核验 | 不需要数据库 |
| `.../exact/postgres` | 持久化 facilitator、进程内客户端和 HTTP 处理器 | 通过 `pgx` 使用 PostgreSQL |

## 快速上手

以下合约地址均为 Nile 示例。在生产环境中，**每个部署地址和运行时代码哈希都必须自行独立核验**；SDK 不会从节点或付款请求中获取这些值。

### 客户端

用官方 HTTP 客户端访问受 x402 保护的资源并自动付款：

```go
mechanism, err := exact.NewClient(exact.ClientConfig{
    Authorization: exact.Config{
        Network:        tron.NetworkNile,
        Permit2Address: "TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h",
        ProxyAddress:   "TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F",
    },
    PayerAddress: signer.Address(),
    Signer:       signer.SignAuthorization, // 也可以是你自己的 TIP-712 摘要签名器
    Asset:        asset,                    // 愿意支付的 TRC-20 代币
    PayTo:        payTo,                    // 信任的收款人
    MaxAmount:    "1000",                   // 单次付款上限（最小单位）
})
if err != nil {
    return err
}

network := x402.Network(mechanism.Network())
core := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{
    AllowedAssets: []x402.SpendControlAsset{{Network: network, Asset: asset, MaxAmountPerPayment: "1000"}},
})).Register(network, mechanism)

client := x402http.WrapHTTPClientWithPayment(&http.Client{Timeout: 15 * time.Second}, x402http.Newx402HTTPClient(core))
```

付款方需要事先为 Permit2 合约授予 TRC-20 授权额度，SDK 不会自动 approve。详见[客户端指南](guide/client.zh-CN.md)。

### 资源服务端

生成 TRON 报价，只在结算成功后才执行业务处理：

```go
mechanism, err := exact.NewServer(exact.ServerConfig{Network: tron.NetworkNile, Asset: asset, PayTo: payTo})
if err != nil {
    return err
}

network := x402.Network(mechanism.Network())
routes := x402http.RoutesConfig{"GET /resource": {Accepts: []x402http.PaymentOption{{
    Scheme: "exact", Network: network, PayTo: payTo, Price: "1000", MaxTimeoutSeconds: 60,
}}}}
core := x402.Newx402ResourceServer(x402.WithFacilitatorClient(facilitator)).Register(network, mechanism)
server := x402http.Wrappedx402HTTPResourceServer(routes, core)
if err := server.Initialize(ctx); err != nil {
    return err
}
handler := nethttp.PaymentMiddlewareFromHTTPServer(server)(mux)
```

价格使用最小单位的字符串，例如 `"1000"`；不接受美元金额或浮点数价格。详见[资源服务端指南](guide/resource-server.zh-CN.md)。

### Facilitator

把持久化 facilitator 直接嵌入资源服务端，不需要额外的服务：

```go
store, err := postgres.New(pool, contracts) // 需要事先执行 postgres.SchemaSQL
if err != nil {
    return err
}
mechanism, err := postgres.NewFacilitator(store, postgres.FacilitatorConfig{
    Preflight:       exact.PreflightConfig{Authorization: contracts, NodeURL: jsonRPCURL, Permit2CodeHash: permit2Hash, ProxyCodeHash: proxyHash},
    Execution:       exact.ExecutionConfig{Network: tron.NetworkNile, NodeURL: fullNodeURL, GenesisBlockID: genesisID},
    Policy:          exact.ServerConfig{Network: tron.NetworkNile, Asset: asset, PayTo: payTo},
    ExecutorAddress: executor,        // 支付 TRX 手续费的账户
    Signer:          signTransaction, // 对交易摘要签名
    MaxAmount:       "1000",
    FeeLimitSUN:     20_000_000,
    Lifetime:        time.Minute,
})
if err != nil {
    return err
}
facilitator, err := postgres.NewLocalClient(mechanism, 8) // 传给 x402.WithFacilitatorClient
```

如果要把 facilitator 作为独立服务运行，可以使用 `postgres.NewHTTPHandler`。配置项、数据库 schema 和恢复语义详见 [facilitator 指南](guide/facilitator.zh-CN.md)。

## 工作原理

```
客户端                      资源服务端                      Facilitator                     TRON
  │  GET /resource              │                              │                              │
  │ ──────────────────────────▶ │                              │                              │
  │  402 + TRON exact 报价      │                              │                              │
  │ ◀────────────────────────── │                              │                              │
  │  签署 Permit2 见证授权      │                              │                              │
  │  GET + PAYMENT-SIGNATURE    │                              │                              │
  │ ──────────────────────────▶ │  settle                      │                              │
  │                             │ ───────────────────────────▶ │  验签、预检、模拟            │
  │                             │                              │  记日志 → 签名 → 广播        │
  │                             │                              │ ───────────────────────────▶ │
  │                             │                              │  固化 + 精确转账核验         │
  │                             │  success + txid              │ ◀─────────────────────────── │
  │  200 + PAYMENT-RESPONSE     │ ◀─────────────────────────── │                              │
  │ ◀────────────────────────── │                              │                              │
```

付款方在链下签署 Permit2 授权；facilitator 的执行账户通过 x402 exact Permit2 代理合约提交结算并支付 TRX 手续费。资源要等转账最终确认后才会交付。

## 示例

| 示例 | 说明 |
| --- | --- |
| [`examples/client`](examples/client) | 用临时密钥演示离线 402 握手，不访问区块链。运行方式：`go run ./examples/client` |
| [`examples/server`](examples/server) | 使用官方 `net/http` 中间件的资源服务端 |
| [`examples/facilitator`](examples/facilitator) | 带鉴权的 facilitator HTTP 服务，仅限测试网和本机监听 |
| [`examples/payer`](examples/payer) | 测试网付款端，`pay` / `recover` 可安全应对崩溃 |
| [`examples/deployment`](examples/deployment) | 只读检查合约部署和代码哈希 |

每个示例的配置说明见各指南，全部变量都列在 [`.env.example`](.env.example) 中。可运行的 API 示例还有 [`exact/example_test.go`](exact/example_test.go) 和 [`exact/postgres/example_test.go`](exact/postgres/example_test.go)。

## 文档

| 指南 | 内容 |
| --- | --- |
| [客户端](guide/client.zh-CN.md) | 注册方式、签名器、限额，以及一笔有效付款能证明和不能证明什么 |
| [资源服务端](guide/resource-server.zh-CN.md) | 报价、`upfront` 流程、facilitator 能力发现和示例服务配置 |
| [Facilitator](guide/facilitator.zh-CN.md) | 嵌入或独立部署、付款示例、结算日志和恢复机制 |
| [底层组件](guide/components.zh-CN.md) | 离线验签、预检、部署检查、交易构建、广播和固化核验 |
| [测试与验证](guide/validation.zh-CN.md) | 测试套件、互操作测试、Nile 实付记录、可复现的合约构建和打包 |
| [威胁模型](THREAT_MODEL.md) | 资产、信任边界、威胁、防护措施和残余风险 |

## 互操作

离线测试双向对接已发布的 `@bankofai/x402-tron@2.1.2` 和 `@x402/core@2.29.0`。两个项目都以十进制 CAIP-2 标识为规范格式（例如 `tron:3448148188`），并接受旧的十六进制标识作为别名。

Nile 测试网上已完成双向实付：从 TypeScript 付款端到 Go facilitator，以及从 Go 付款端到 TypeScript facilitator。回执格式和重放行为方面的差异记录在[测试与验证](guide/validation.zh-CN.md#nile-实付记录)中，其中一项已反馈给上游：[BofAI/x402#122](https://github.com/BofAI/x402/issues/122)。

## 开发

```bash
go mod verify
go vet ./...
go test -race ./exact/... .
```

设置 `X402_TEST_DATABASE_URL` 指向专用测试库后，会运行 PostgreSQL 集成测试，否则自动跳过。互操作测试需要 Node.js 24.22.1 或更高版本。CI 还会运行 `govulncheck` 并构建发布包。详见[测试与验证](guide/validation.zh-CN.md)。

欢迎贡献。较大的改动请先开 issue 讨论；提交 PR 前，请对改动的包运行 `gofmt`、`go vet` 和相关测试。

## 安全

请通过 [GitHub Security Advisories](https://github.com/eightyun/x402-tron-go/security/advisories/new) 私下报告漏洞，不要使用公开 issue。详见 [SECURITY.md](SECURITY.md) 和[威胁模型](THREAT_MODEL.md)。

使用时请注意：

- 单次付款上限不是累计预算，授权也不绑定具体 URL。请在应用中限制可信来源和总支出。
- 超时或 pending 不代表付款失败。请用同一个授权恢复，不要重新生成授权。
- Facilitator 信任其配置的节点和数据库，不验证 Merkle 证明，也无法防御恶意节点。

## 许可证

基于 [Apache License 2.0](LICENSE) 发布。第三方组件见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
