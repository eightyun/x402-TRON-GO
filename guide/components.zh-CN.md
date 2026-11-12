# 底层组件

简体中文 | [English](components.md)

各机制使用的底层组件。多数接入方不会直接调用；本文说明它们执行的具体检查与边界。

## 地址与网络工具

- 识别 Mainnet、Nile、Shasta 网络，兼容已知历史十六进制别名，统一输出十进制 CAIP-2 标识。
- 校验 Base58Check 或 `41` 前缀十六进制地址，支持两者转换。
- 将有效 TRON 地址显式转换成 TIP-712 所需的 `0x` + 20 字节地址。
- 根包仅使用 Go 标准库；`exact` 包复用官方 SDK v2.30.0 的协议类型，使用成熟的 secp256k1/Keccak 库校验授权。
- 离线校验不访问 RPC；可选的 `PreflightVerifier` 仅执行只读 JSON-RPC。两者均不持有密钥、不批准代币授权、不消费 nonce 或广播交易。

网络标识支持不等于该网络的支付机制已实现。TRON 主网和测试网使用相同地址格式，**不能根据地址判断网络**。地址格式有效不代表已激活、筛查通过或拥有资金。

### 用法

要求 Go 1.26.10 或更高版本；CI 使用最新 1.26.x 补丁版。当前仅有测试网候选版本，公共 API 仍可能调整。

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

batch付款预检先合并链ID/初始头与部分合约读取，再补余下合约和付款状态，减少一轮往返；末组中其他只读查询可能在某项拒绝前已经发送，但所有检查必须通过，才单独执行完整模拟及末头核对。默认模式保持原阶段闸门。任一模式都不是原子快照，也不保证公共RPC每轮可用。

默认保持单请求并行；可在`PreflightConfig`设`BatchRPC: true`（部署检查/facilitator示例设`X402_RPC_BATCH=true`）减少独立检查的HTTP往返。batch不是链上Multicall或原子快照：逻辑读取、完整settle模拟、前后同头和时效检查完全保留；最多6项一组、总响应1MiB，拒绝缺失/重复/未知id或错误响应，不自动降级/重试。公共节点延迟仍可能导致`chain_state_changed`，不能通过关闭校验规避；`chain_read_failed`、`chain_head_stale`、`chain_network_mismatch`、`chain_contract_mismatch`为固定脱敏分类。

`exact.NewPreflightVerifier` 接受 `PreflightConfig`，其 `Authorization` 沿用离线校验的可信合约配置。必须另行提供可信 `NodeURL`、`Permit2CodeHash` 和 `ProxyCodeHash`；两项哈希为 runtime bytecode 的 Keccak-256（`0x` + 32 字节），必须从独立核验的部署资料获得，不能自动信任节点第一次返回的代码。资产地址仍由服务端可信报价决定。

`Check(ctx, payload, requirements, now)` 会：

1. 先完成离线授权校验，再查询节点链 ID 和最新链头。
2. 核对 Permit2/代理代码哈希、代理 `PERMIT2()` 与二字段 `WITNESS_TYPEHASH()`、Permit2 `DOMAIN_SEPARATOR()`；要求代币地址存在代码。
3. 查询余额、对 Permit2 的 allowance，以及 `nonceBitmap(payer, nonce >> 8)` 中对应的 nonce 位。
4. 再次核对链 ID/链头，并把 RPC 用时计入最后的授权有效期检查。

余额或 allowance 不足、nonce 已使用、代码/绑定不符、头变化或任何读取失败都会拒绝。成功返回的 `PreflightResult` 只有授权与观测头，**不是结算证明，也不会锁定余额或 nonce**。不提供恶意节点的状态证明；配置节点和合约代码哈希仍是信任边界。

`CheckWithSimulation(ctx, payload, requirements, executorAddress, now)` 在上述 nonce 检查后、后置链头/时效检查前，额外通过 `eth_call` 模拟完整 `settle`。费用付款方必须由部署方固定，不能取自付款请求；调用的代理、授权、收款人、签名和 ABI 编码与原生交易构建器一致，附带 TRX 为零。固定 ABI 无返回值，只有精确的 `"0x"` 被接受；节点不支持、revert、读取失败或异常返回都会拒绝，不静默降级。响应拒绝重复键、错误信封和尾随 JSON，错误不回显节点原文。

原 `Check` 保持 13 次读取；带完整模拟的一轮为 14 次，共用默认 15 秒、上限 1 分钟的总预算，不自动重试。独立只读查询分组并发：链 ID 与链头、六项合约检查、余额/allowance/nonce，完整模拟仍在前置检查全部通过后单独执行。每个预检器共用最多6个在途 JSON-RPC 额度，排队也计入预算，组内任务全部退出后才返回；取消/超时保留 context 语义，其他并发错误按原逻辑顺序选择，不因响应快慢改变分类。失败组中的其他只读请求可能已发送，但不会进入后续模拟/签名/广播。

自定义 `HTTPClient`/Transport 须支持并发并遵守 context 取消。单请求与显式batch均按逻辑读取数共用6额度，batch不把并发放大成36读。该上限不是跨多个预检器的全局限流或累计费用预算，突发请求仍可能触发节点429并被拒绝；没有Multicall合约依赖、缓存或失败时自动降级。最新链头仍最多落后服务端60秒或超前5秒，前后链头仍须一致；较慢节点上仍可能被保守拒绝，调用方可在自身预算内重新预检，但不能据此自动发起新交易。

自建 facilitator 的 `/verify` 和新付款 `/settle` **强制完整模拟**，使用配置的 `ExecutorAddress`；模拟失败返回 `settlement_simulation_failed`，不会新增占用、签名或广播。该行为比原状态预检严格，不支持完整调用的节点不能接纳新付款；已经广播/待确认/成功的历史记录仍只恢复原交易，不因已消费 nonce 或模拟不可用而再次付款。模拟成功不代表账户权限、Energy/Bandwidth 或费用资源充分，也不预留资金、不证明实发或固化到账。

TRON 的 [eth_call](https://developers.tron.network/reference/eth_call) 和 [eth_getCode](https://developers.tron.network/reference/eth_getcode) 当前读取 latest 状态，不能通过传入区块号获得历史状态。前后链头一致只是漂移检查，**不是原子快照或最终性保证**。结算前仍须复查，最终以实际链上执行结果为准。

默认要求 HTTPS 并禁止重定向；API key 仅通过 `TRON-PRO-API-KEY` 头发送，节点 URL 不允许携带凭据、query 或 fragment。`HTTPClient` 可用于部署方可信 CA/代理配置，不会被原地修改；明文 HTTP 仅在显式启用 `AllowInsecureLocalHTTP` 后用于 loopback/localhost 测试节点。节点配置绝不能取自付款请求。

## 无钱包部署检查

同一 `PreflightVerifier` 提供 `CheckDeployment(ctx, assetAddress, now)`：无需付款人、签名或有效报价，即可检查配置的链 ID、鲜活链头、Permit2/代理代码哈希、代币非空代码、`PERMIT2()`、两字段 `WITNESS_TYPEHASH()` 和 `DOMAIN_SEPARATOR()`，最后再次核对链/头/新鲜度。正常仍为10次只读请求，按“边界2读→合约6读→边界2读”分组并发，使用原预检总预算/共享6请求上限，不自动重试。返回 `DeploymentResult`，原付款预检和结算逻辑不变。

可运行 [部署检查示例](../examples/deployment/main.go)：显式设置下列环境变量，再执行 `go run ./examples/deployment`。

| 变量 | 含义 |
| --- | --- |
| `X402_NETWORK` | 默认 Nile；仅允许 Nile/Shasta，拒绝主网及其别名 |
| `X402_JSON_RPC_URL` | 必填，可信 HTTPS JSON-RPC；禁止 URL 凭据、query 和重定向 |
| `X402_PERMIT2_ADDRESS` / `X402_PROXY_ADDRESS` | 必填，独立核验的部署地址，无默认值 |
| `X402_PERMIT2_CODE_HASH` / `X402_PROXY_CODE_HASH` | 必填，独立可信的 runtime bytecode Keccak-256，无自动学习 |
| `X402_ASSET` | 必填，待检查的非零 TRON 代币地址，不得为上述两个合约 |
| `X402_NODE_API_KEY` | 可选，只放请求头，不输出 |

命令总预算15秒；成功输出结构化网络/资产/被检查合约（`41` 前缀 hex）及配置代码哈希/观测头、`readOnly: true` 和 `paymentValidated: false`，失败非零退出并仅给脱敏分类。不会加载钱包、连接数据库、查询用户资金、生成授权、模拟 `settle`、批准代币授权或广播。

**这不是完整部署或付款验收。** 代码哈希必须先从独立可信资料核验，不能将节点本次返回值直接当作期望哈希。代币只检查非空代码，未验证精度/转账语义；也未检查原生节点创世块/固化接口、完整 `settle` 模拟能力、账户权限或费用资源。部署结果不能传作付款成功证明，也不能取代逐笔 `CheckWithSimulation` 和固化到账核验。

## 受限结算交易构建与外部签名

`exact.NewSettlementBuilder` 显式配置可信 `Authorization`、费用付款方 `ExecutorAddress`、正数 `FeeLimitSUN` 和 `Lifetime`（大于 15 秒、不超过 10 分钟、整毫秒）。无默认主网/合约配置，不持有私钥，不信任节点提供的任意交易 JSON。

`Build` 先校验付款授权，再以可信 `ReferenceBlock` 和服务端时间本地构建单笔 `exact` 代理 `settle` 调用、TAPOS 和 TRON raw Protobuf，交易 ID 为 `SHA-256(raw_data)`。参考区块网络/高度/ID 必须一致，时间最多落后 60 秒或超前 5 秒；这些离线检查不证明区块真实归属或固化性，节点与合约部署仍需独立预检。交易过期不晚于付款授权 deadline，构建/签名前后至少剩余 15 秒。

返回的 `PreparedSettlement` 不可变，`RawData()` 返回副本（其中包含付款授权签名，仍须保密）。`Sign(ctx, DigestSigner, now)` 仅向调用方管理的外部签名器传递固定 SHA-256 摘要，并恢复公钥核对费用付款方；签名耗时计入有效期复查，错误不回显签名器原始消息。支持低 s、65 字节 `r||s||v`，外部返回 `v=0/1` 或 `27/28`，输出统一为 `0/1`。付款人 TIP-712 授权与费用付款人交易签名是两份不同签名，不能混用摘要。

`SettlementTransaction.Payload` 是**完整二进制 Transaction Protobuf，不是 JSON**，适用于后续 [broadcasthex](https://developers.tron.network/reference/broadcasthex) 的十六进制请求。`ValidateSigned` 只接受对应准备对象的规范编码，核对所有 raw 字节、ID 与签名，拒绝未知/重复字段、附带 TRX/TRC10、额外合约/签名或替换费用策略；不是通用 TRON 交易导入器，也不检查当前有效期或证明到账。

目前只支持默认 owner 权限的单 EOA 费用付款方，不支持多签、自定义权限或合约钱包。仍须核验链上账户权限、费用资源和执行结果；构建器不加载密钥，也不自行判定结算成功。签名成功后必须通过持久化日志保存唯一交易，再取得广播许可；重试不能延长原交易生命周期或另行构建付款。独立公开向量 [tronweb-settlement.json](../exact/testdata/tronweb-settlement.json) 使用合成区块和已丢弃的随机测试密钥，仅验证编码/签名互操作，不是测试网上链验收。

## 单次广播与原交易查询

`exact.NewExecutionClient` 接受可信 `ExecutionConfig`：固定 `Network`、原生 FullNode 根 `NodeURL` 和独立核验的 `GenesisBlockID`（无 `0x` 的 64 位 hex，前 8 字节为零，末 4 字节匹配 TRON chain ID）。不能从付款请求选节点或自动学习创世 ID；测试中的合成 ID 不可作为部署配置。默认 HTTPS，仅显式启用的 loopback 节点允许明文；URL 不接受 path/query/fragment/userinfo。超时默认 15 秒、最多 1 分钟；响应最多 1 MiB，不跟随重定向，不回显 URL、API key 或原始节点消息。自定义 HTTP Transport 属于可信配置，不得自行重试写请求。

- `Broadcast(ctx, prepared, transaction, now)` 是低层单次写接口：必须先持久化并取得日志许可。它校验完整字节/签名/网络/剩余有效期，向**同一个原生节点**查询区块 0 并比较完整创世 ID，再发一次 `/wallet/broadcasthex`，最后复核网络。只有明确 `result=true`、成功 code（或兼容缺省）及原 txid 一致才表示本次受理；不是成功结算。网络读取用时计入广播前有效期检查。
- `Lookup(ctx, txid)` 前后核对同节点创世 ID，查询 `/wallet/gettransactionbyid`，核对返回 txID 并重算 `SHA-256(raw_data_hex)`。返回 `TransactionObservation.Found`，**不代表成功执行、固化或收款到账**。只有空对象表示本次未找到；超时、歧义/重复顶层 JSON 字段、错误 hash 或网络不符都返回错误。未找到或请求被拒绝均不能据此释放占用、重签或另建付款。

这些请求已用于 [Nile 实付记录](validation.zh-CN.md#nile-实付记录)中的真实结算，自动化测试仍使用模拟节点。节点受理、FullNode 主体、执行回执和固化付款事件是不同证据层次；前两个接口不判成功，需使用下面的只读交叉核验，也不提供恶意节点的状态证明。

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

恢复后必须将 `prepared.Authorization()` 与原日志的可信授权标识完全比较，再调用只读固化核验。仅接受本包的单一合约调用、单一 EOA 签名和固定规范编码，其他合法 TRON 交易也可能拒绝；不是通用历史交易导入器。[持久化结算日志](facilitator.zh-CN.md#持久化结算日志)中的 `ConfirmSettlement` 串联恢复、核验及成功持久化，facilitator 及 HTTP 示例均使用该流程；单笔实付不覆盖所有中断窗口，不能把模拟测试当作上线验收。
