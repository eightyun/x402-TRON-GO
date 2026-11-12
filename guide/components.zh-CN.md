# 底层组件

简体中文 | [English](components.md)

本文介绍 `exact` 机制所依赖的底层组件。多数接入方不会直接调用它们。如果你需要直接使用这些组件，或想准确了解每一步检查什么、不保证什么，请阅读本文。

## 依赖与副作用

- 根包 `github.com/eightyun/x402-tron-go` 仅使用 Go 标准库。
- `exact` 包复用官方 x402 Go SDK v2.30.0 的协议类型，并使用成熟的 secp256k1 和 Keccak 库校验签名。
- 离线校验不发起任何 RPC 调用。
- 可选的 `PreflightVerifier` 仅执行只读 JSON-RPC 调用。
- 这些组件均不持有密钥、不批准代币授权、不消费 nonce，也不广播交易。广播只能通过下文单独介绍的 `ExecutionClient.Broadcast` 完成。

## 地址与网络工具

| 函数 | 用途 |
| --- | --- |
| `NormalizeNetwork` | 将 Mainnet、Nile、Shasta 标识规范化为十进制 CAIP-2 格式，兼容已知的历史十六进制别名。 |
| `ChainID` | 返回已知网络的链 ID。 |
| `NormalizeAddressHex` | 校验地址，返回 42 位 `41` 前缀小写十六进制格式。 |
| `NormalizeAddressBase58` | 校验地址，返回规范 Base58Check 格式。 |
| `AddressForSigning` | 将有效 TRON 地址显式转换成 TIP-712 所需的 20 字节 `0x` 地址。 |

能识别某个网络标识，不代表该网络已支持结算。

主网和测试网使用相同的地址格式，**不能根据地址判断网络**。地址格式有效，也不代表账户已激活、安全或拥有资金。

### 用法

要求 Go 1.26.10 或更高版本，CI 使用最新的 1.26.x 补丁版。目前仅有测试网候选版本，公共 API 仍可能调整。

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

### 输入严格性

- 不会隐式删除空白。
- 拒绝未知网络、通配符和带前导零的十进制网络标识。
- 网络命名空间必须是小写 `tron`；历史十六进制引用不区分大小写。
- 地址函数只接受 34 位 Base58Check 或 42 位 `41` 前缀十六进制输入。
- `AddressForSigning` 输出 20 字节 `0x` 地址，但不接受任意 EVM 地址作为输入。
- 格式转换不等于签名验证。

## exact Permit2 离线校验

`exact.NewOfflineVerifier` 接受显式的可信配置：一个 TRON 网络、Permit2 合约和 exact 代理地址。

- 不提供默认主网配置。
- 合约地址绝不取自付款人传入的 `extra` 字段。
- 部署方仍需独立核验合约字节码，以及域与代理的绑定关系。

### 方法

- `Digest` 计算二字段 `Witness(to, validAfter)` 的 TIP-712 摘要，供外部签名器使用。
- `VerifyAuthorization` 接受官方 SDK 的 `types.PaymentPayload`、服务端可信的 `types.PaymentRequirements` 和服务端当前时间。它绑定报价、网络、代币、金额、收款人、代理、nonce 和有效期，然后恢复 EOA 公钥并核对其与付款人一致。

### 支持的子集

这是一套限定的互操作子集，不是对所有 x402 或 TRON 扩展的完整支持。

- `extra` 必须包含 `assetTransferMethod: "permit2"`。
- `extra` 可额外包含 `paymentFlow: "upfront"`。不接受其他字段、流程或非空 `extensions`。
- 实际付款与可信报价中的 `paymentFlow` 必须一致。
- 配置和报价中的地址使用严格的 TRON 格式；授权中的地址使用严格的 20 字节 `0x` 格式。
- 金额和时间为规范十进制 uint256；nonce 可为十进制或 32 字节 `0x` 十六进制。
- 截止时间须至少剩余 6 秒，且不超过报价 timeout。
- 仅接受 65 字节低 s 的 EOA 签名，`v` 可为 0/1 或 27/28。不支持合约钱包签名。

> [!WARNING]
> **离线授权有效，不代表付款人能够扣款，也不代表款项已到账。** 校验不读取余额、allowance、nonce 位图或其他链上状态，不消费 nonce，也不提供并发或防重放保护，因此重复校验同一授权可以成功。**不能据此交付付费资源。** 后续仍须完成链上预检、持久化幂等、结算和最终结果核验。

此授权不绑定具体的 HTTP URL 或请求内容。资源服务端还需防止同一付款被用于多次履约。

### 测试向量

[`exact/testdata/tronweb-permit2.json`](../exact/testdata/tronweb-permit2.json) 是使用 TronWeb 6.3.0 独立生成并验签的公开向量。它使用的临时随机测试密钥已丢弃。该向量只离线验证摘要和签名兼容性，不是完整的客户端或 facilitator 互操作验收，也未涉及真实钱包或区块链。

## 只读链上预检

`exact.NewPreflightVerifier` 将离线校验与只读链上状态检查组合在一起。它不持有密钥、不消费 nonce，也不广播。同一个预检器可并发使用。

### 配置

`PreflightConfig` 是可信的部署配置，绝不能取自付款请求。

| 字段 | 含义 |
| --- | --- |
| `Authorization` | 可信合约配置，与离线校验相同。 |
| `NodeURL` | 可信 JSON-RPC 端点。 |
| `APIKey` | 可选，仅通过 `TRON-PRO-API-KEY` 请求头发送。 |
| `Permit2CodeHash`、`ProxyCodeHash` | runtime bytecode 的 Keccak-256，格式为 `0x` + 32 字节。必须取自独立核验的部署资料，绝不在首次使用时从节点学习。 |
| `Timeout` | 每次检查的总预算，默认 15 秒，上限 1 分钟。 |
| `HTTPClient` | 可选，用于可信 CA 或代理配置，不会被原地修改。 |
| `AllowInsecureLocalHTTP` | 显式允许明文 HTTP，仅限 loopback 或 localhost 开发节点。 |
| `BatchRPC` | 显式启用 JSON-RPC batch 模式，见下文。 |

连接规则：

- 默认要求 HTTPS。
- 禁止重定向。
- URL 不得包含凭据、query 或 fragment。

代币地址始终来自服务端的可信报价。

### `Check`

`Check(ctx, payload, requirements, now)` 依次执行：

1. 离线校验授权，然后读取链 ID 和最新链头。
2. 检查链头新鲜度：最多落后服务端时间 60 秒，或超前 5 秒。
3. 核对 Permit2 和代理的 runtime 代码哈希；核对代理的 `PERMIT2()` 与二字段 `WITNESS_TYPEHASH()`，以及 Permit2 的 `DOMAIN_SEPARATOR()`；要求代币地址存在代码。
4. 读取付款人的代币余额、对 Permit2 的 allowance，以及 `nonceBitmap(payer, nonce >> 8)` 中对应的 nonce 位。
5. 再次读取链 ID 和链头。链头必须与第一次相同，且仍然新鲜。
6. 把 RPC 用时计入后，再次检查授权有效期。

读取失败、余额或 allowance 不足、nonce 已使用、合约或绑定不符、网络不符或链头变化，都会直接拒绝。

成功返回的 `PreflightResult` 只包含已校验的授权和观测到的链头。**它不是结算证明，也不会锁定余额或 nonce。** 配置的 RPC 节点和独立核验的代码哈希仍是信任边界。不提供针对恶意节点的状态证明。

### `CheckWithSimulation`

`CheckWithSimulation(ctx, payload, requirements, executorAddress, now)` 执行相同的检查，并在 nonce 检查之后、最后的链头和有效期检查之前，额外通过 `eth_call` 模拟完整的 `settle` 调用。

- `executorAddress` 是费用付款方，必须是固定的部署配置，绝不能来自付款输入。
- 模拟使用的代理、授权、收款人、签名和 ABI 编码与原生交易构建器一致，不附带 TRX。
- 固定 ABI 没有返回值，因此只接受精确的 `"0x"` 结果。
- 节点不支持该方法、revert、读取失败或返回异常都会拒绝，不做降级。
- 拒绝包含重复键、错误信封或尾随 JSON 的响应。错误不回显节点原始信息。

模拟成功不代表账户权限、Energy、Bandwidth 或费用充足，也不预留资金，不证明交易已广播或付款已固化。

### 请求数、并发与重试

- `Check` 执行 13 次读取，`CheckWithSimulation` 执行 14 次。两者共用同一个总 `Timeout`，均不自动重试。
- 独立读取按组并发：先链 ID 与链头，再六项合约检查，然后是余额、allowance 和 nonce。完整模拟单独执行，且只在前置检查全部通过后进行。
- 每个预检器最多允许 6 个在途 JSON-RPC 请求，由其所有调用方共享。排队时间也计入预算。
- 每组任务全部结束后才返回。
- 取消和超时保留 context 语义。其他错误按检查的原始逻辑顺序报告，而不是按响应到达的先后。
- 组内某项检查失败时，同组其他只读请求可能已经发出，但不会进入后续阶段：不模拟、不签名、不广播。

6 个并发的上限按预检器实例计算，不是跨多个预检器的全局限流，也不是累计费用预算。突发请求仍可能触发节点返回 HTTP 429，并被直接拒绝。没有 Multicall 合约依赖、缓存或自动降级。

自定义的 `HTTPClient` 或 Transport 必须支持并发使用，并遵守 context 取消。

较慢的节点仍可能导致检查被保守拒绝。调用方可以在自身预算内重新预检，但预检失败或重试绝不意味着可以发起另一笔交易。

### JSON-RPC batch 模式

默认使用单请求并行读取。可设置 `PreflightConfig.BatchRPC: true` 以减少 HTTP 往返。部署检查和 facilitator 示例通过 `X402_RPC_BATCH=true` 启用。

batch 模式下，付款预检分两批发送，比默认模式少一轮往返：

1. 链 ID、初始链头和其中四项合约读取。
2. 剩余两项合约读取，以及余额、allowance 和 nonce。

默认模式保持原有的分阶段闸门。batch 模式只改变请求的发送方式：

- 它是 JSON-RPC batch，不是链上 Multicall，也不是原子快照。
- 所有逻辑读取、完整 settle 模拟、前后链头一致及新鲜度检查都必须执行。
- 每批最多 6 项查询，总响应上限 1 MiB。
- 缺失、重复或未知的 id，以及错误响应，都会直接拒绝。不降级，也不自动重试。
- 单请求与 batch 共用同样的 6 个逻辑读取额度，batch 不会把额度放大成 36 次读取。
- 某项检查拒绝时，同批其他只读查询可能已经执行。所有检查仍须全部通过，才会单独执行完整模拟和最后的链头比对。

两种模式都不保证公共 RPC 每次都可用。

### 失败原因

较慢的公共节点仍可能导致 `chain_state_changed`。不要通过关闭校验来规避。其他固定的脱敏原因为：

- `chain_read_failed`
- `chain_head_stale`
- `chain_network_mismatch`
- `chain_contract_mismatch`

### 在自建 facilitator 中的使用

自建 facilitator 的 `/verify` 和新付款的 `/settle` **始终要求完整模拟**，并使用配置的 `ExecutorAddress`。

- 模拟失败返回 `settlement_simulation_failed`，不会创建占用，也不签名或广播。
- 这比单纯的状态预检更严格：无法模拟完整调用的节点不能接纳新付款。
- 已广播、待确认或已成功的记录只恢复原交易，不会再次模拟；已消费的 nonce 或不可用的模拟也绝不会导致第二次付款。

### 链上状态语义

TRON 的 [eth_call](https://developers.tron.network/reference/eth_call) 和 [eth_getCode](https://developers.tron.network/reference/eth_getcode) 读取的是 latest 状态，无法通过传入区块号获得历史状态。

> [!WARNING]
> 读取前后链头一致只是漂移检查，**不是原子快照，也不是最终性保证**。结算前必须重新校验，并核验实际的链上执行结果。

## 无钱包部署检查

同一个 `PreflightVerifier` 还提供 `CheckDeployment(ctx, assetAddress, now)`。它不需要付款人、签名或有效报价，依次执行：

1. 读取链 ID 和新鲜链头。
2. 核对固定的 Permit2 和代理 runtime 代码哈希，要求代币存在非空代码；核对 `PERMIT2()`、二字段 `WITNESS_TYPEHASH()` 和 `DOMAIN_SEPARATOR()`。
3. 再次读取链 ID 和链头，确认链头未变化且仍然新鲜。

正常一轮执行 10 次只读请求，按组并发：2 次边界读取，6 次合约读取，再 2 次边界读取。它沿用相同的总预算和共享的 6 个在途请求上限，不自动重试。资产地址不得与两个已配置合约相同。

返回值为 `DeploymentResult`。原有的付款校验和结算逻辑不变。

### 运行示例

设置下列环境变量，然后执行 `go run ./examples/deployment` 运行 [部署检查示例](../examples/deployment/main.go)。

| 变量 | 含义 |
| --- | --- |
| `X402_NETWORK` | 默认 Nile。仅允许 Nile 和 Shasta，拒绝主网及其别名。 |
| `X402_JSON_RPC_URL` | 必填。可信的 HTTPS JSON-RPC 端点，不允许 URL 凭据、query 和重定向。 |
| `X402_PERMIT2_ADDRESS` / `X402_PROXY_ADDRESS` | 必填。独立核验的部署地址，无默认值。 |
| `X402_PERMIT2_CODE_HASH` / `X402_PROXY_CODE_HASH` | 必填。独立可信的 runtime bytecode Keccak-256 哈希，绝不自动学习。 |
| `X402_ASSET` | 必填。待检查的非零 TRON 代币地址，不得与两个已配置合约相同。 |
| `X402_NODE_API_KEY` | 可选。只通过请求头发送，绝不输出。 |
| `X402_RPC_BATCH` | 可选。`true` 启用 JSON-RPC batch 模式。只接受 `true`、`false` 或空值。 |

命令的总预算为 15 秒。

- 成功时输出结构化结果：网络、资产、被检查的合约（`41` 前缀 hex）、配置的代码哈希和链头观测，并带有 `readOnly: true` 和 `paymentValidated: false`。
- 失败时以非零状态退出，只输出脱敏的分类。

该命令不会加载钱包、连接数据库、查询用户资金、生成授权、模拟 `settle`、批准代币授权或广播。

> [!WARNING]
> **这不是完整的部署或付款验收。** 期望的代码哈希必须先从独立可信的资料核验，绝不能把节点本次的返回值当作期望值。代币只检查存在非空代码，不验证精度或转账语义。原生节点的创世块和固化接口、完整 `settle` 的模拟能力、账户权限和费用资源均未检查。该结果不能证明付款成功，也不能取代逐笔的 `CheckWithSimulation` 和固化到账核验。

## 受限结算交易构建与外部签名

`exact.NewSettlementBuilder` 用于构建本包结算所用的唯一一种交易形态。它接受 `SettlementBuilderConfig`：

| 字段 | 含义 |
| --- | --- |
| `Authorization` | 可信合约配置。 |
| `ExecutorAddress` | 支付 gas 的账户，必须显式设置。 |
| `FeeLimitSUN` | 以 SUN 计的费用上限，必须为正数。 |
| `Lifetime` | 交易生命周期，必须为整毫秒，大于 15 秒且不超过 10 分钟。 |

不提供默认主网或合约配置。构建器不持有私钥，也绝不接受节点构建的任意交易 JSON。

### `Build`

`Build` 先校验付款授权，再使用可信的 `ReferenceBlock` 和服务端时间，在本地构建一笔 exact 代理 `settle` 调用、TAPOS 字段和 TRON raw Protobuf。交易 ID 为 `SHA-256(raw_data)`。

- 参考区块的网络、高度和 ID 必须一致。
- 参考区块时间最多落后 60 秒，或超前 5 秒。
- 交易过期时间不晚于付款授权的 deadline。
- 构建时须至少剩余 15 秒有效期，签名前后也各须至少剩余 15 秒。

这些离线检查不能证明区块真实属于该链、已具备最终性，也不能证明 RPC 响应诚实。部署检查和链上预检仍然必需。

### `PreparedSettlement` 与 `Sign`

返回的 `PreparedSettlement` 不可变。

- `RawData()` 返回 raw 数据的副本。其中包含付款人的授权签名，须保密。
- `Sign(ctx, DigestSigner, now)` 只向调用方的外部签名器发送固定的 SHA-256 摘要，然后恢复公钥并核对其属于 gas 付款方。
- 签名耗时计入有效期。不回显签名器的原始错误信息。
- 签名为低 s、65 字节的 `r||s||v`。签名器返回的 `v` 可为 0/1 或 27/28，输出统一规范化为 0/1。

付款人的 TIP-712 授权与执行方的交易签名是两份不同的签名，二者的摘要不能混用。

### `SettlementTransaction` 与 `ValidateSigned`

`SettlementTransaction.Payload` 是**完整的二进制 Transaction Protobuf，不是 JSON**，可用于后续十六进制编码的 [broadcasthex](https://developers.tron.network/reference/broadcasthex) 请求。

`ValidateSigned` 只接受对应准备对象的规范编码，核对全部 raw 字节、ID 和签名。以下情况会被拒绝：

- 未知或重复字段
- 附带 TRX 或 TRC10
- 额外的合约或签名
- 被修改的费用策略

它不是通用交易导入器，不检查当前有效期，也不证明到账。

### 范围限制

- 只支持使用默认 owner 权限的单个 EOA。不支持多签、自定义权限或合约钱包。
- 仍须核验链上账户权限、费用资源和执行结果。
- 构建器绝不加载密钥，也不判定结算是否成功。
- 签名后，必须先持久化唯一的已签名交易并从日志取得广播许可，再提交。恢复期间绝不能延长生命周期或另建付款。

公开的 [TronWeb 结算向量](../exact/testdata/tronweb-settlement.json) 使用合成区块和已丢弃的随机执行方密钥，只验证编码和签名互操作性，不是测试网上链验收。

## 单次广播与原交易查询

`exact.NewExecutionClient` 接受可信的 `ExecutionConfig`：

| 字段 | 含义 |
| --- | --- |
| `Network` | 固定的 TRON 网络。 |
| `NodeURL` | 原生 FullNode 的根 URL。除非显式启用 loopback 明文 HTTP，否则必须使用 HTTPS。不允许 path、query、fragment 或 userinfo。 |
| `GenesisBlockID` | 独立核验的创世区块 ID：不带 `0x` 的 64 位 hex，前 8 字节为零，末 4 字节与 TRON 链 ID 一致。 |
| `APIKey` | 可选，仅通过 `TRON-PRO-API-KEY` 请求头发送。 |
| `Timeout` | 总预算，默认 15 秒，上限 1 分钟。 |
| `HTTPClient` | 可选的自定义客户端。 |
| `AllowInsecureLocalHTTP` | 仅允许 loopback 节点使用明文 HTTP。 |

- 绝不根据付款请求选择节点，也绝不在首次使用时学习创世身份。
- 测试中的合成 ID 不能作为部署配置。
- 响应上限 1 MiB，禁止重定向。
- 错误中不包含 URL、API key 或节点原始信息。
- 自定义 HTTP Transport 属于可信配置，不得在内部重试写请求。

### `Broadcast`

`Broadcast(ctx, prepared, transaction, now)` 是低层的单次写接口，只能在持久化日志授予许可后调用。

1. 校验完整字节、签名、网络和剩余有效期。
2. 从**同一个原生节点**读取区块 0，并比较完整创世 ID。
3. 只发送一次 `/wallet/broadcasthex` 请求。
4. 再次核对网络身份。

只有明确受理、code 成功（或缺省）且 txid 与原交易一致时，才确认本次尝试。这只表示节点受理了本次广播，不代表结算成功。网络检查的用时计入发送前的有效期检查。

### `Lookup`

`Lookup(ctx, txid)` 在调用 `/wallet/gettransactionbyid` 前后都核对创世 ID，并校验返回的 txID、重新计算 `SHA-256(raw_data_hex)`。

- `TransactionObservation.Found` **不代表执行成功、已具备最终性或已收款**。
- 只有空对象表示当前未找到。
- 超时、有歧义或重复的顶层 JSON 键、错误的 hash 以及网络不符都返回错误。
- 无论是未找到还是请求被拒绝，都不能据此释放占用、重新签名或另建付款。

### 证据层次

这些请求已在 [Nile 实付记录](validation.zh-CN.md#nile-实付记录)的真实结算中使用过。自动化测试仍使用模拟节点。

节点受理、FullNode 交易主体、执行回执和固化付款事件是不同层次的证据。`Broadcast` 和 `Lookup` 都不判定成功，请使用下文的只读核验。不提供针对恶意节点的状态证明。

## 原交易固化与付款事件核验

`executionClient.CheckSettlement(ctx, prepared, transaction, time.Now())` 使用**原准备对象和原已签名交易**。它绝不重新构建过期授权、不广播，也不写入日志或账本。已过期的交易和授权仍可核验其历史结果。`now` 必须是可信的服务端时间。

配置的端点必须提供 `/walletsolidity` API。核验器在一个有界总预算内：

1. 核对 solidity 视图的创世 ID，并读取新鲜的固化链头。
2. 读取固化的交易主体和回执，逐字节核对原 raw 数据、txid 及其 SHA-256、单合约执行结果和回执身份。
3. 核对回执的区块高度和时间、初始固化链头边界，以及该区块内恰好包含一笔原交易。同一高度下，链头 ID 与区块 ID 必须一致。
4. 要求恰好存在一笔从付款人到收款人、金额精确的标准代币 `Transfer`。少付、超付、重复付款、费用扣减，以及涉及任一方的其他同币种转入或转出，均会被拒绝。绝不对金额求和，也绝不把毛流量当作净到账。
5. 复核链头没有回退，原区块 ID、时间和交易归属未变，以及网络身份，然后返回 `SettlementObservation.Confirmed=true` 及区块 ID 和高度。

### 结果

- 读取不到固化主体或回执，或回执刚好超出初始链头时，返回 `Confirmed=false`。这不是最终失败。
- 节点报告执行未成功、证据矛盾、网络错误、超时或关键字段无效时返回错误。**绝不会返回 `Confirmed=true`，也不能据此释放占用、重新签名或重新广播。**

### 预算与新鲜度

- 成功的核验通常执行 9 次串行只读请求，不重试。
- 沿用相同的限制：默认总超时 15 秒（上限 1 分钟）、1 MiB 响应上限、TLS 和重定向限制，以及错误脱敏。
- 关键的嵌套 JSON 对象拒绝重复键。原生整数字段拒绝字符串、浮点数和溢出。
- 固化链头最多落后可信时间 2 分钟，或超前 5 秒。总耗时也计入初始快照的新鲜度检查。
- 慢节点、长时间停链或超大区块可能导致核验被保守拒绝。只能在调用方预算内重做只读核验。

### 范围限制

- 只支持可信的标准 TRC-20 代币。不支持自转账、fee-on-transfer、rebase 或自定义事件语义。
- 可信节点以及独立核验的合约和代币部署仍是前提假设。
- 不验证完整区块链，不提供 Merkle 证明，也不防御恶意节点状态。
- 事件的含义取决于可信的代币实现。

固化视图和日志的用途参见 [TRON 确认语义](https://developers.tron.network/docs/confirmation-semantics)和[事件编码](https://developers.tron.network/docs/event)。

### 重启后恢复

`exact.RestoreSettlement(config, executorAddress, originalTransaction)` 仅从持久化的原已签名交易恢复 `PreparedSettlement`。`config` 和费用付款方必须来自可信的部署配置。

它执行以下检查：

1. 严格解码本包固定的 Protobuf 和 ABI。
2. 重新编码，并与原交易逐字节比较。
3. 核对付款人的 TIP-712 签名、执行方的交易签名和原 txid。

授权字段取自原调用，绝不取自额外的历史快照或当前报价。

恢复绝不调用 `Build`，绝不伪造当前时间来接受过期授权，也绝不续期、签名或广播。TAPOS 字节不是完整的参考区块证明。恢复本身也不能证明原报价、预检或付款成功。

在调用只读固化核验之前，须将 `prepared.Authorization()` 与原日志记录中的可信授权标识进行比较。

只接受本包固定的规范编码：单一合约调用、单一 EOA 签名。其他合法的 TRON 交易也可能被拒绝。它不是通用的历史交易导入器。

[持久化结算日志](facilitator.zh-CN.md#持久化结算日志)中的 `ConfirmSettlement` 串联了恢复、核验和成功结果的持久化，facilitator 及其 HTTP 示例都使用该流程。单笔实付不能覆盖所有中断窗口，模拟测试也不能当作部署验收。
