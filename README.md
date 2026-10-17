# x402-tron-go

简体中文 | [English](README.en.md)

面向 x402 的独立 TRON Go 适配 SDK，目标是让客户端/Agent、API 服务端和 facilitator 直接使用 TRON 支付，不必自行实现链适配，也不依赖我们的支付网关。当前已有官方客户端、资源服务端和 PostgreSQL facilitator 机制，以及授权预检、交易构建/广播、固化精确到账核验和持久化恢复。**facilitator HTTP 启动示例及真实测试网闭环仍未完成，不可用于承载真实资金**。本项目不隶属于 x402 Foundation 或 TRON。

## 开箱即用进度

| 角色 | 当前状态 |
| --- | --- |
| 客户端 / Agent | 已实现官方 `SchemeNetworkClient`；配置付款策略和签名器，由官方 HTTP 客户端处理 402 与签名重试 |
| API 资源服务端 | 已实现官方 `SchemeNetworkServer`、Permit2 报价与 `upfront` 中间件示例；仍需可信 facilitator，不能仅验签就发货 |
| 自建 facilitator | 已实现官方 `SchemeNetworkFacilitator`，串联预检、受控签名/广播、有界确认与原交易恢复；待 HTTP 启动示例和真实测试网验收 |

普通客户端不需要 PostgreSQL，不加载支付网关的商户/账本模块。数据库只是自建 facilitator 可选的持久化实现。网络、资产与收款策略、钱包签名、Gas 和必要的 Permit2 代币授权仍是接入前置条件；不自动授权或隐式上链。

## 客户端快速体验

要求 Go 1.26 或更高；尚未发布稳定版本。克隆仓库后运行：

```bash
go run ./examples/client
```

该示例使用进程内临时随机密钥和 loopback HTTP 夹具，真实调用官方 x402 SDK：`402 → 生成 TRON 授权 → PAYMENT-SIGNATURE 重试 → 202 已验签、未结算`。不会连接节点、读取既有钱包、发起交易或返回成功结算回执。示例地址仅是公开协议夹具，不能作为可信部署地址；这不是完整链上闭环。

集成已有应用时，可直接注册本项目的机制，无需自己拼授权字段、nonce 或 TIP-712 摘要：

```go
import (
    "net/http"
    "time"

    "github.com/eightyun/x402-tron-go/exact"
    x402 "github.com/x402-foundation/x402/go/v2"
    x402http "github.com/x402-foundation/x402/go/v2/http"
)

// contracts 必须来自独立核验的可信部署配置，金额均为代币最小单位。
func paymentHTTPClient(contracts exact.Config, signer *exact.EOASigner, asset, payTo, maxAmount string) (*http.Client, error) {
    mechanism, err := exact.NewClient(exact.ClientConfig{
        Authorization: contracts, PayerAddress: signer.Address(),
        Asset: asset, PayTo: payTo, MaxAmount: maxAmount,
        Signer: signer.SignAuthorization,
    })
    if err != nil {
        return nil, err
    }
    network := x402.Network(mechanism.Network())
    core := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{
        AllowedAssets: []x402.SpendControlAsset{{Network: network, Asset: asset, MaxAmountPerPayment: maxAmount}},
    })).Register(network, mechanism)
    return x402http.WrapHTTPClientWithPayment(&http.Client{
        Timeout: 15 * time.Second,
        CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
    }, x402http.Newx402HTTPClient(core)), nil
}
```

`exact.NewEOASigner(keyBytes)` 是可选的内存 EOA 签名器，严格接受 32 字节非零有效私钥；调用者管理原始密钥并在结束后 `Close()`。不读取 env/文件、不导出密钥，常见格式化及 JSON 不输出内部标量；尽力清除内存不等于安全硬件。生产可通过 `ClientConfig.Signer` 接入自己的外部签名器，输入是**已计算的 TIP-712 摘要**，不能再哈希、加 personal_sign 前缀或混用交易 SHA-256 签名。

客户端在签名前匹配配置的网络、资产、收款人、单次 `MaxAmount` 和官方 SDK 传入的金额上限，拒绝未知 extra/非空扩展。`Lifetime` 默认 60 秒，允许 6 秒至 10 分钟整秒，实际期限取本地与报价较小值；签名后复验签名和有效期。外部签名器应支持并发并遵守有界 context，错误不回显原文。客户端不查询余额/allowance，资产配置不能取自不可信 402 报价。

**单次上限不是累计预算。** 每次创建载荷都会产生新的可扣款授权，不能对未知结果自动重新付款；现有授权也不绑定 URL/请求内容。应用仍须限制目标站点/预算，避免不可信重定向或额外恢复 hooks 重复授权。HTTP 2xx 或本地验签结果均不是链上到账证明。

## API 资源服务端接入

`exact.NewServer(exact.ServerConfig{Network: network, Asset: asset, PayTo: payTo})` 可直接注册到官方资源服务端。固定单网络/资产/收款人，不需要数据库或钱包私钥。`ParsePrice` 接受规范最小单位正整数字符串（如 `"1000"`）、官方 `x402.AssetAmount` 或相应严格 map；**不接受 `$0.01`、浮点数或隐式汇率换算**。

机制只启用官方 `upfront`：报价自动包含 `assetTransferMethod: permit2` 和 `paymentFlow: upfront`，facilitator 结算明确成功后才运行 API handler。`pending`、`unknown`、拒绝、错误或超时都不能返回结算成功。upfront 不先调用独立的 `/verify`，所以 **`/settle` 必须自行完整验签、预检、持久化并核验固化执行与实际到账**；不能只广播就返回 `success=true`。

启动时要求可信 facilitator 的 `/supported` 包含匹配的 `x402Version: 2`、`scheme: exact`、规范网络，以及 `extra.supportedAssetTransferMethods` 数组中的 `permit2`。这个能力字段沿用已有 TRON 生态实现，不会被复制到支付报价；声明本身不是可信合约或付款证明。合约部署配置仍需在客户端和 facilitator 独立核对。

可运行的 [服务端示例](examples/server/main.go) 使用官方 net/http 中间件。先设置以下环境变量，再运行 `go run ./examples/server`：

| 变量 | 含义 |
| --- | --- |
| `X402_NETWORK` | 默认 Nile：`tron:3448148188` |
| `X402_ASSET` | 必填，可信代币地址 |
| `X402_PAY_TO` | 必填，商户收款地址 |
| `X402_PRICE_ATOMIC` | 必填，单次价格最小单位正整数 |
| `X402_FACILITATOR_URL` | 必填，可信 HTTPS facilitator 基础 URL，无 userinfo/query/fragment |
| `X402_LISTEN_ADDR` | 默认 `127.0.0.1:8080` |

示例启动前显式 `Initialize` 并构建报价，配置/能力校验失败就退出，不忽略错误继续监听。提供 `GET /resource`，未付款返回 402；HEAD 被拒绝，不能利用 Go GET 路由的 HEAD 匹配绕过。配置了有界启动/结算/HTTP 超时、禁上游重定向、请求头上限和优雅关闭。对外部署应加 TLS 入口及适当鉴权；第三方 facilitator 认证应使用官方 `AuthProvider`，不要把 token 拼进 URL。

**本项目尚无经过真实测试网验收的 facilitator HTTP 服务**。已有 PostgreSQL 机制可注册到官方 SDK，配套 HTTP 启动示例仍待补齐；运行资源服务端示例需另行配置经过验证的服务，不默认使用公共地址。资源服务端门槛测试使用测试专用 facilitator，机制测试则使用真实实现加模拟链节点；两者都不是链上验收证明。

先结算意味着业务 handler 随后失败时不会自动退款，也不应再次结算；应用需设计履约幂等、查询和补偿。新服务端的 upfront 报价需要本次新版客户端；原有不带 paymentFlow 的报价和签名仍兼容。授权摘要不绑定 paymentFlow，校验器与官方服务端会核对可信报价和 accepted 一致，不能通过剥离该字段降级流程。

## 当前能力

- 识别 Mainnet、Nile、Shasta 网络，兼容已知历史十六进制别名，统一输出十进制 CAIP-2 标识。
- 校验 Base58Check 或 `41` 前缀十六进制地址，支持两者转换。
- 将有效 TRON 地址显式转换成 TIP-712 所需的 `0x` + 20 字节地址。
- 根包仅使用 Go 标准库；`exact` 包复用官方 SDK v2.28.0 的协议类型，使用成熟的 secp256k1/Keccak 库校验授权。
- 离线校验不访问 RPC；可选的 `PreflightVerifier` 仅执行只读 JSON-RPC。两者均不持有密钥、不批准代币授权、不消费 nonce 或广播交易。

网络标识支持不等于该网络的支付机制已实现。TRON 主网和测试网使用相同地址格式，**不能根据地址判断网络**。地址格式有效不代表已激活、筛查通过或拥有资金。

## 使用

要求 Go 1.26 或更高版本；CI 使用 1.26.x。尚未发布版本，公共 API 仍可能调整。

```go
import tron "github.com/eightyun/x402-tron-go"

network, err := tron.NormalizeNetwork("tron:0xcd8690dc")
if err != nil {
    return err
}
// network 为 tron:3448148188

address, err := tron.NormalizeAddressHex("TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL")
if err != nil {
    return err
}
// address 为 418840e6c55b9ada326d211d818c34a994aeced808
```

输入为严格格式：不隐式删除空白，不接受未知网络、通配符或带前导零的十进制网络标识。网络命名空间必须是小写 `tron`；历史十六进制引用不区分大小写。

地址工具只接受 34 位 Base58Check 或 42 位 `41` 前缀十六进制。`AddressForSigning` 输出 20 字节 `0x` 地址，但不反向接受任意 EVM 地址。不要把格式转换当作签名验证。

## exact Permit2 离线校验

`exact.NewOfflineVerifier` 显式配置一个 TRON 网络、Permit2 合约和 exact 代理地址；不从付款人传入的 `extra` 获取合约，也不提供默认主网配置。部署方仍需独立核验合约字节码、域与代理绑定。

- `Digest` 计算二字段 `Witness(to, validAfter)` 的 TIP-712 摘要，供外部签名器使用。
- `VerifyAuthorization` 接受官方 SDK 的 `types.PaymentPayload`、服务端可信 `types.PaymentRequirements` 和服务端当前时间；绑定报价、网络、资产、金额、收款人、代理、nonce 和有效期，并恢复 EOA 公钥核对付款人。
- `extra` 必须包含 `assetTransferMethod: "permit2"`，可增加唯一的 `paymentFlow: "upfront"`；拒绝其他字段、流程与非空 `extensions`，并核对两侧 paymentFlow 一致。这是一套限定的互操作子集，不是所有 x402/TRON 扩展的完整支持。
- 配置及报价地址使用 TRON 格式，授权中的地址使用严格的 20 字节 `0x` 格式。金额/时间使用规范十进制 uint256；nonce 也支持 32 字节 `0x` 编码。截止时间须至少剩余 6 秒且不超过报价 timeout。
- 仅支持 65 字节低 s 的 EOA 签名，`v` 可为 0/1 或 27/28；尚不支持合约钱包签名。

**离线授权有效 ≠ 可扣款 ≠ 已到账。** 校验不读取余额、allowance、nonce 位图或链状态，不消费 nonce，也不提供并发防重放。重复离线校验同一授权可以成功。**不能据此交付付费资源**；后续必须完成链上预检、持久化幂等、结算和最终结果核验。

此授权不绑定具体 HTTP URL 或请求内容，资源服务端还需防止同一付款被用于重复履约。

`exact/testdata/tronweb-permit2.json` 是 TronWeb 6.3.0 独立生成并验签的公开向量。使用临时随机测试密钥，密钥未保存；这只验证摘要/签名兼容，不是完整客户端或真实链上验收。

## 只读链上预检

`exact.NewPreflightVerifier` 接受 `PreflightConfig`，其 `Authorization` 沿用离线校验的可信合约配置。必须另行提供可信 `NodeURL`、`Permit2CodeHash` 和 `ProxyCodeHash`；两项哈希为 runtime bytecode 的 Keccak-256（`0x` + 32 字节），必须从独立核验的部署资料获得，不能自动信任节点第一次返回的代码。资产地址仍由服务端可信报价决定。

`Check(ctx, payload, requirements, now)` 会：

1. 先完成离线授权校验，再查询节点链 ID 和最新链头。
2. 核对 Permit2/代理代码哈希、代理 `PERMIT2()` 与二字段 `WITNESS_TYPEHASH()`、Permit2 `DOMAIN_SEPARATOR()`；要求代币地址存在代码。
3. 查询余额、对 Permit2 的 allowance，以及 `nonceBitmap(payer, nonce >> 8)` 中对应的 nonce 位。
4. 再次核对链 ID/链头，并把 RPC 用时计入最后的授权有效期检查。

余额或 allowance 不足、nonce 已使用、代码/绑定不符、头变化或任何读取失败都会拒绝。成功返回的 `PreflightResult` 只有授权与观测头，**不是结算证明，也不会锁定余额或 nonce**。不提供恶意节点的状态证明；配置节点和合约代码哈希仍是信任边界。

当前一轮正常预检执行 13 次串行读取。总预算默认 15 秒、上限 1 分钟，不自动重试；最新链头最多落后服务端 60 秒或超前 5 秒。较慢节点上可能因链头更新而被保守拒绝，调用方可在自身预算内重新预检，但不能据此自动发起新交易。

TRON 的 [eth_call](https://developers.tron.network/reference/eth_call) 和 [eth_getCode](https://developers.tron.network/reference/eth_getcode) 当前读取 latest 状态，不能通过传入区块号获得历史状态。前后链头一致只是漂移检查，**不是原子快照或最终性保证**。结算前仍须复查，最终以实际链上执行结果为准。

默认要求 HTTPS 并禁止重定向；API key 仅通过 `TRON-PRO-API-KEY` 头发送，节点 URL 不允许携带凭据、query 或 fragment。`HTTPClient` 可用于部署方可信 CA/代理配置，不会被原地修改；明文 HTTP 仅在显式启用 `AllowInsecureLocalHTTP` 后用于 loopback/localhost 测试节点。节点配置绝不能取自付款请求。

## 受限结算交易构建与外部签名

`exact.NewSettlementBuilder` 显式配置可信 `Authorization`、费用付款方 `ExecutorAddress`、正数 `FeeLimitSUN` 和 `Lifetime`（大于 15 秒、不超过 10 分钟、整毫秒）。无默认主网/合约配置，不持有私钥，不信任节点提供的任意交易 JSON。

`Build` 先校验付款授权，再以可信 `ReferenceBlock` 和服务端时间本地构建单笔 `exact` 代理 `settle` 调用、TAPOS 和 TRON raw Protobuf，交易 ID 为 `SHA-256(raw_data)`。参考区块网络/高度/ID 必须一致，时间最多落后 60 秒或超前 5 秒；这些离线检查不证明区块真实归属或固化性，节点与合约部署仍需独立预检。交易过期不晚于付款授权 deadline，构建/签名前后至少剩余 15 秒。

返回的 `PreparedSettlement` 不可变，`RawData()` 返回副本（其中包含付款授权签名，仍须保密）。`Sign(ctx, DigestSigner, now)` 仅向调用方管理的外部签名器传递固定 SHA-256 摘要，并恢复公钥核对费用付款方；签名耗时计入有效期复查，错误不回显签名器原始消息。支持低 s、65 字节 `r||s||v`，外部返回 `v=0/1` 或 `27/28`，输出统一为 `0/1`。付款人 TIP-712 授权与费用付款人交易签名是两份不同签名，不能混用摘要。

`SettlementTransaction.Payload` 是**完整二进制 Transaction Protobuf，不是 JSON**，适用于后续 [broadcasthex](https://developers.tron.network/reference/broadcasthex) 的十六进制请求。`ValidateSigned` 只接受对应准备对象的规范编码，核对所有 raw 字节、ID 与签名，拒绝未知/重复字段、附带 TRX/TRC10、额外合约/签名或替换费用策略；不是通用 TRON 交易导入器，也不检查当前有效期或证明到账。

目前只支持默认 owner 权限的单 EOA 费用付款方，不支持多签、自定义权限或合约钱包。仍须核验链上账户权限、费用资源和执行结果；构建器不加载密钥，也不自行判定结算成功。签名成功后必须通过持久化日志保存唯一交易，再取得广播许可；重试不能延长原交易生命周期或另行构建付款。独立公开向量 [tronweb-settlement.json](exact/testdata/tronweb-settlement.json) 使用合成区块和已丢弃的随机测试密钥，仅验证编码/签名互操作，不是测试网上链验收。

## 单次广播与原交易查询

`exact.NewExecutionClient` 接受可信 `ExecutionConfig`：固定 `Network`、原生 FullNode 根 `NodeURL` 和独立核验的 `GenesisBlockID`（无 `0x` 的 64 位 hex，前 8 字节为零，末 4 字节匹配 TRON chain ID）。不能从付款请求选节点或自动学习创世 ID；测试中的合成 ID 不可作为部署配置。默认 HTTPS，仅显式启用的 loopback 节点允许明文；URL 不接受 path/query/fragment/userinfo。超时默认 15 秒、最多 1 分钟；响应最多 1 MiB，不跟随重定向，不回显 URL、API key 或原始节点消息。自定义 HTTP Transport 属于可信配置，不得自行重试写请求。

- `Broadcast(ctx, prepared, transaction, now)` 是低层单次写接口：必须先持久化并取得日志许可。它校验完整字节/签名/网络/剩余有效期，向**同一个原生节点**查询区块 0 并比较完整创世 ID，再发一次 `/wallet/broadcasthex`，最后复核网络。只有明确 `result=true`、成功 code（或兼容缺省）及原 txid 一致才表示本次受理；不是成功结算。网络读取用时计入广播前有效期检查。
- `Lookup(ctx, txid)` 前后核对同节点创世 ID，查询 `/wallet/gettransactionbyid`，核对返回 txID 并重算 `SHA-256(raw_data_hex)`。返回 `TransactionObservation.Found`，**不代表成功执行、固化或收款到账**。只有空对象表示本次未找到；超时、歧义/重复顶层 JSON 字段、错误 hash 或网络不符都返回错误。未找到或请求被拒绝均不能据此释放占用、重签或另建付款。

实际请求只有模拟节点测试覆盖，尚未运行真实 Nile 广播。节点受理、FullNode 主体、执行回执和固化付款事件是不同证据层次；前两个接口不判成功，需使用下面的只读交叉核验，也不提供恶意节点的状态证明。

## 原交易固化与付款事件核验

`executionClient.CheckSettlement(ctx, prepared, transaction, time.Now())` 使用**原准备对象和已签名原交易**。不重新构建过期授权、不广播、不改账或日志。交易/授权已经过期仍可核验历史结果；`now` 必须来自可信服务端。

同一配置端点须提供 `/walletsolidity` API。核验器在有界总预算内：

1. 核对 solidity 视图的完整创世 ID 与新鲜固化头。
2. 读取固化交易和回执，逐字节核对原 raw、txid/SHA-256、单笔合约执行结果和回执身份。
3. 核对回执块高/时间、固化头边界，以及该区块内恰好一笔原交易；同高度头/块 ID 必须一致。
4. 要求标准代币的付款人→收款人出现唯一一笔精确金额 Transfer。少付、超付、重复、费用扣减或涉及任一方的其他同资产转入/转出均拒绝，不求和、不把毛额当净到账。
5. 复核头不回退、原块 ID/时间/原交易归属不变和节点网络身份，再返回 `SettlementObservation.Confirmed=true` 及区块 ID/高度。

读取不到固化主体/回执，或交易刚超出首个固化头时，仅返回 `Confirmed=false`，不是最终失败。节点报告执行不成功、证据矛盾、网络错误、超时或关键字段无效返回错误，**不会返回 Confirmed=true，也不能据此释放占用、重签或重播**。

成功通常需要 9 次串行只读请求，无内部重试。沿用 15 秒默认总预算（最多 1 分钟）、1 MiB 响应上限、TLS/重定向限制和错误脱敏。关键嵌套 JSON 对象拒绝重复字段；原生整数字段不接受浮点、字符串或溢出。固化头最多落后可信时间 2 分钟、超前 5 秒；原始快照的耗时也计入最后新鲜度检查。慢节点、长停链或超限大块可能被保守拒绝，调用者只能在预算内重做只读核验。

**范围限制：**只适用于可信标准 TRC20 代币，不支持自转账、fee-on-transfer、rebase 或自定义事件语义。核验依赖可信节点与独立核对的合约/资产部署，不验证完整区块链、Merkle 证明或恶意节点状态。事件的链上含义须由可信代币实现保证。[TRON 官方确认语义](https://developers.tron.network/docs/confirmation-semantics)和[事件编码](https://developers.tron.network/docs/event)说明了固化视图与日志的用途。

重启后可调用 `exact.RestoreSettlement(config, executorAddress, originalTransaction)`，仅从持久化的原签名交易恢复 `PreparedSettlement`。`config` 和费用付款方必须来自可信部署配置。严格解码本包固定的 Protobuf/ABI，重编码逐字节比对，并重新核对付款人的 TIP-712 签名、费用付款人的交易签名和原 txid；授权参数从原调用提取，不采信额外历史 JSON 或当前报价。恢复不调用 `Build`，不伪造当前时间验证过期授权，不续期、不签名、不广播。TAPOS 字节不等于完整参考块证明；恢复本身也不证明原报价、预检或到账成功。

恢复后必须将 `prepared.Authorization()` 与原日志的可信授权标识完全比较，再调用只读固化核验。仅接受本包的单一合约调用、单一 EOA 签名和固定规范编码，其他合法 TRON 交易也可能拒绝；不是通用历史交易导入器。下面的 `ConfirmSettlement` 串联恢复、核验及成功持久化；facilitator 机制已经使用该流程，但 HTTP 部署与真实测试网验收仍待完成，不能把模拟测试当作上线验收。

## 持久化结算日志（可选 PostgreSQL）

`exact/postgres.New` 接受调用方管理的 `pgxpool.Pool` 和可信 `exact.Config`。先在专用、可信的 `search_path` 下显式应用 [schema.sql](exact/postgres/schema.sql)（也可通过 `SchemaSQL` 获取），构造器不自动执行 DDL。未导入该包的工具/验签消费者不需要数据库；日志也不依赖支付网关或商户账本。

**已有日志库必须先升级：**备份并停止旧版写入，在同一可信 `search_path` 下显式执行 [upgrade_confirmation.sql](exact/postgres/upgrade_confirmation.sql)（`UpgradeConfirmationSQL`），然后启动新版。脚本以事务新增确认列并扩展约束，可重复执行、不改历史交易或状态；失败须回滚未完成事务。仅重跑 `SchemaSQL` 不会升级旧表。运行账号不需要 DDL 权限；不要混跑不认识成功终态的旧版本，也不要通过删除列、清确认结果或降级状态来回滚。

- `Reserve` 先离线验签，再按网络、Permit2 合约、付款人和规范化 nonce 占用；相同授权返回原记录，不同签名摘要冲突。不同代理不能绕过同一个 Permit2 nonce 空间。
- `SaveTransaction` 只能保存一份交易 ID 和签名字节。并发提交只有一份不同结果能够获胜；相同内容可重试，已保存的内容不可替换，同网络的一个 txid 不能绑定其他授权。
- `BeginBroadcast` 先原子提交 `broadcast_unknown`，再返回唯一一次广播许可及持久化的原交易。只有该调用成功才能广播；不能广播尚未保存的本地结果。
- 节点受理后调用 `RecordAccepted` 进入 `pending`，不因受理就判成功；如果原记录已经 `succeeded`，相同 txid/摘要的迟到回调幂等保留成功及确认时间。超时、进程退出、查不到交易或数据库提交结果不明，都只能通过 `Get` 恢复并追踪原 txid，不得重签、替换或自动新建付款。广播之前崩溃也可能留下 unknown，当前没有自动重播或释放机制。
- 推荐通过 `Submit(ctx, id, prepared, executionClient, now)` 串联提交：从数据库读原字节、校验、CAS 标记 unknown、广播原快照、记录受理。并发/重复提交只有一次写许可，已 unknown/pending/succeeded 不再发送。节点已受理但 ACK 落库失败仍保留 unknown/原 txid，不回退到可发送状态。SQL 提交本身不确定时，必须重新 `Get`，不能假定未广播。
- `Track(ctx, id, executionClient)` 重启后只用日志中的原 txid 查询，不需重建过期授权/Prepare；完全只读，不因 Found 而修改为成功或因未找到而重播。调用方应对整轮数据库和网络操作设置有界 context；本库不启动后台执行器、轮询或队列。
- `store.CheckSettlement(ctx, id, executorAddress, executionClient, now)` 从 unknown/pending/succeeded 原记录恢复准备对象，先核对可信合约、费用付款方及原授权标识，再核验固化原交易和精确到账。不需要保留内存准备对象；过期后仍可历史核验。只读，不修改状态/更新时间，不重播；无效或不匹配的旧交易字节在读取节点前拒绝。成功后的再次审计即使未确认或报错，也不自动撤销既有成功。
- `store.ConfirmSettlement(ctx, id, executorAddress, executionClient, now)` 在内部完成上述核验后，用原授权/txid/签名字节的 CAS 将 unknown/pending 原子转为 `succeeded`，保存 `Record.Confirmation` 的区块高度、区块 ID 和数据库确认时间。没有外部“传成功布尔值/回执就改状态”的入口。并发相同结果幂等，不同区块冲突不覆盖。未固化返回 `ErrSettlementUnconfirmed`；任何错误都不能当作成功，提交响应丢失也要先 `Get` 复核，不能重新付款。
- 已成功记录的 `ConfirmSettlement` 会先恢复并核对原绑定/可信配置，再返回已存结果，不重新查询节点，不更新首次确认时间；需要当前节点审计时显式用 `CheckSettlement`。`SaveTransaction` 的相同原字节重试及迟到受理回调不会降级成功，`BeginBroadcast`/`Submit` 不再提供广播许可。整轮 SQL、恢复、RPC 和提交必须由调用方的有界 context 约束。

这些约束只对共享同一可信数据库、遵循上述流程的实例生效。`Reserve` 不锁定链上 nonce/余额，`SaveTransaction` 本身仍将交易字节视为不透明数据：应使用可信构建器完成 ID/签名/授权绑定/有效期校验，不能直接接收付款请求提供的任意交易。`succeeded` 是可信节点核验结果的持久化，不是独立链上证明；有写权限的数据库仍是信任边界。**成功记录本身不绑定 URL/业务请求，也不授权重复交付资源**；HTTP 部署鉴权、业务请求与报价的关联及履约幂等仍由接入方负责，没有“恰好一次链上付款/资源交付”的保证。合约预检、账户权限、费用资源和风控闸门仍是必要条件。

签名数据可被广播，数据库须最小权限、加密与备份；`Transaction.Payload` 不参与 JSON 序列化，仍禁止自行打印。运行账号不应拥有 DELETE/TRUNCATE 权限，不自动清理过期占用；回滚代码也不能删除历史日志。CI 的 localhost 临时测试库使用 trust 认证，**不是部署配置**。

## 官方 facilitator 机制（可选 PostgreSQL）

使用 `postgres.NewFacilitator(store, config)` 创建机制，再以 `x402.Newx402Facilitator().Register([]x402.Network{x402.Network(mechanism.Network())}, mechanism)` 注册官方 SDK。无需自行实现 TRON 验签、原生交易构建或结算状态机。构造不联网、不迁移数据库、不调用签名器，也不启动 HTTP 或后台服务。

`FacilitatorConfig` 需显式提供：

| 配置 | 含义 |
| --- | --- |
| `Preflight` | 可信授权域、JSON-RPC 节点、独立核验的 Permit2/代理代码哈希 |
| `Execution` | 同网络原生节点根地址、独立核验的完整创世 ID；端点必须提供 wallet 和 walletsolidity |
| `Policy` / `MaxAmount` | 固定网络、单资产、单收款人及单笔最小单位上限，不接受请求覆盖 |
| `ExecutorAddress` / `Signer` | 默认 owner 权限的 EOA 费用付款方及外部摘要签名器；必须遵守 context，禁止签名器自行广播 |
| `FeeLimitSUN` / `Lifetime` | 正数单笔费用上限、原交易生命周期（大于15秒、不超过10分钟、整毫秒） |
| `Timeout` / `PollInterval` | 整轮默认90秒、最多3分钟；确认轮询默认3秒，范围1~10秒 |

- 只支持 v2/exact/Permit2/upfront，报价超时6~600秒，新付款仍须满足构建器至少15秒剩余有效期；能力声明为 `supportedAssetTransferMethods: ["permit2"]`，签名人声明为配置的费用付款方。各网络/合约配置必须与 Store 一致。当前不支持动态多商户路由、其他 scheme、扩展、多签或合约钱包。
- `Verify` 是只读新请求预检，不写日志、预留 nonce、签名或广播，也不保证之后结算一定成功。
- `Settle` 不依赖先 `Verify`。新请求完整验签/策略/时效/链上预检后占用日志；单行事务锁内再次读取状态、构建、签名并提交唯一原交易，事务成功后才能广播。预检、广播和最终性等待不持有该事务锁；并发实例竞争失败后追踪原交易，不重新付款。
- 已 unknown/pending/succeeded 的原授权只做可信报价/签名/持久化身份匹配及原交易恢复，不再按当前时效拒绝历史请求或查询已消费的 nonce。底层 `VerifyAuthorizationBinding` 仅作历史识别，不能代替新请求的 `VerifyAuthorization`/`Preflight.Check` 准入。
- 只有 `ConfirmSettlement` 核验且成功落库后，官方响应才包含 `success=true` 和原 txid/network/payer/amount。缺失固化证据在预算内轮询；预算耗尽、广播未知或核验节点暂不可用，以携带原 txid 的官方 `settlement_pending` 错误返回。提交状态不明时返回 `settlement_recovery_required`，先读回日志；都不能据此释放占用、重签已保存交易或创建新付款。
- 官方资源服务端仅对带原 txid 的 pending **原样自动重试一次**，不是无限轮询。机制自身负责有界等待。远程 facilitator 客户端、反向代理及 HTTP 服务超时应覆盖整轮机制预算；现有资源服务端示例的15秒远程调用超时可能先中断等待，下一步提供配套 facilitator HTTP 启动示例。中断后仍须用相同付款载荷恢复，不能重新授权付款。

外部签名期间占用一条数据库连接及行锁，应配置短签名超时和合理连接池。崩溃发生在签名后、保存提交前时，本库从未广播那些未提交字节，后续可能再次调用签名器；**不保证 HSM 调用恰好一次**。所有实例必须共享同一日志并遵循本流程。单笔金额/费用上限不是累计代付预算，公开端点还需身份认证、限流和费用预算；运营方仍须独立核验部署、资产语义、费用资源和账户权限。

## 验证

```bash
go mod verify
go vet . ./exact ./exact/postgres ./examples/client ./examples/server
go test -race -count=1 -cover . ./exact ./exact/postgres ./examples/client ./examples/server
go build . ./exact ./exact/postgres ./examples/client ./examples/server
```

PostgreSQL 集成测试要求 `X402_TEST_DATABASE_URL` 指向专用测试库，并授予创建临时 schema 的权限。每个用例使用独立随机 schema，结束后仅清理自己的 schema；未设置时明确跳过数据库测试，不能当作集成验证通过。CI 配置临时 PostgreSQL 16 并实际执行这些测试。

测试覆盖地址、网络、TronWeb 编码/签名对照、授权/费用/时间边界、模拟节点和 PostgreSQL 并发恢复。固化核验覆盖历史过期交易、完整原始字节/块内归属、执行状态、头/块不一致、精确事件/重复和回转、嵌套重复 JSON、网络/超时/并发；恢复与持久化覆盖升级回滚、约束、冲突结果、迟到回调、SQL 失败和实际提交后响应丢失。facilitator 覆盖官方 SDK 注册/Verify/独立 Settle、动态临时签名、16并发单次签名/广播、过期及已消费 nonce 的历史恢复、pending/节点故障和未提交字节不广播。所有回执和区块均为合成夹具，没有真实链上测试。后续补 facilitator HTTP 启动示例、部署预检与 Nile 闭环，再整理预发布供支付网关引用。

## 参考与发布状态

- [TRON 官方地址格式](https://developers.tron.network/docs/account)
- [x402 官方 Go SDK](https://github.com/x402-foundation/x402/tree/main/go)
- [TRON 生态参考实现](https://github.com/BofAI/x402)

使用 [Apache-2.0](LICENSE) 许可证；第三方参考与依赖见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。尚未发布稳定版本，也未通过独立安全审计。
