# x402-tron-go

简体中文 | [English](README.en.md)

面向 x402 的独立 TRON Go 适配项目，目前提供网络/地址工具、`exact` Permit2 授权校验、只读预检、受限交易构建/签名校验、单次广播与原交易查询，以及可选的持久化日志。**尚未实现完整客户端、facilitator 或链上成功结算核验，也未完成真实测试网闭环，不可用于承载真实资金**。本项目不隶属于 x402 Foundation 或 TRON。

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
- 当前要求 `extra` 仅包含 `assetTransferMethod: "permit2"`，拒绝其他 `extra` 与非空 `extensions`。这是一套明确限定的互操作子集，不是所有 x402/TRON 扩展的完整支持。
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

目前只支持默认 owner 权限的单 EOA 费用付款方，不支持多签、自定义权限或合约钱包。仍须核验链上账户权限、费用资源和执行结果；不提供密钥加载器或成功终态。签名成功后必须通过持久化日志保存唯一交易，再取得广播许可；重试不能延长原交易生命周期或另行构建付款。独立公开向量 [tronweb-settlement.json](exact/testdata/tronweb-settlement.json) 使用合成区块和已丢弃的随机测试密钥，仅验证编码/签名互操作，不是测试网上链验收。

## 单次广播与原交易查询

`exact.NewExecutionClient` 接受可信 `ExecutionConfig`：固定 `Network`、原生 FullNode 根 `NodeURL` 和独立核验的 `GenesisBlockID`（无 `0x` 的 64 位 hex，前 8 字节为零，末 4 字节匹配 TRON chain ID）。不能从付款请求选节点或自动学习创世 ID；测试中的合成 ID 不可作为部署配置。默认 HTTPS，仅显式启用的 loopback 节点允许明文；URL 不接受 path/query/fragment/userinfo。超时默认 15 秒、最多 1 分钟；响应最多 1 MiB，不跟随重定向，不回显 URL、API key 或原始节点消息。自定义 HTTP Transport 属于可信配置，不得自行重试写请求。

- `Broadcast(ctx, prepared, transaction, now)` 是低层单次写接口：必须先持久化并取得日志许可。它校验完整字节/签名/网络/剩余有效期，向**同一个原生节点**查询区块 0 并比较完整创世 ID，再发一次 `/wallet/broadcasthex`，最后复核网络。只有明确 `result=true`、成功 code（或兼容缺省）及原 txid 一致才表示本次受理；不是成功结算。网络读取用时计入广播前有效期检查。
- `Lookup(ctx, txid)` 前后核对同节点创世 ID，查询 `/wallet/gettransactionbyid`，核对返回 txID 并重算 `SHA-256(raw_data_hex)`。返回 `TransactionObservation.Found`，**不代表成功执行、固化或收款到账**。只有空对象表示本次未找到；超时、歧义/重复顶层 JSON 字段、错误 hash 或网络不符都返回错误。未找到或请求被拒绝均不能据此释放占用、重签或另建付款。

实际请求只有模拟节点测试覆盖，尚未运行真实 Nile 广播。节点受理、FullNode 主体、执行回执、固化回执及真实到账是不同证据层次；本轮没有后面三项的成功判定，也不提供恶意节点的状态证明。

## 持久化结算日志（可选 PostgreSQL）

`exact/postgres.New` 接受调用方管理的 `pgxpool.Pool` 和可信 `exact.Config`。先在专用、可信的 `search_path` 下显式应用 [schema.sql](exact/postgres/schema.sql)（也可通过 `SchemaSQL` 获取），构造器不自动执行 DDL。未导入该包的工具/验签消费者不需要数据库；日志也不依赖支付网关或商户账本。

- `Reserve` 先离线验签，再按网络、Permit2 合约、付款人和规范化 nonce 占用；相同授权返回原记录，不同签名摘要冲突。不同代理不能绕过同一个 Permit2 nonce 空间。
- `SaveTransaction` 只能保存一份交易 ID 和签名字节。并发提交只有一份不同结果能够获胜；相同内容可重试，已保存的内容不可替换，同网络的一个 txid 不能绑定其他授权。
- `BeginBroadcast` 先原子提交 `broadcast_unknown`，再返回唯一一次广播许可及持久化的原交易。只有该调用成功才能广播；不能广播尚未保存的本地结果。
- 节点受理后调用 `RecordAccepted` 进入 `pending`，不是成功终态。超时、进程退出、查不到交易或数据库提交结果不明，都只能通过 `Get` 恢复并追踪原 txid，不得重签、替换或自动新建付款。广播之前崩溃也可能留下 unknown，当前没有自动重播或释放机制。
- 推荐通过 `Submit(ctx, id, prepared, executionClient, now)` 串联提交：从数据库读原字节、校验、CAS 标记 unknown、广播原快照、记录受理。并发/重复提交只有一次写许可，已 unknown/pending 不再发送。节点已受理但 ACK 落库失败仍保留 unknown/原 txid，不回退到可发送状态。SQL 提交本身不确定时，必须重新 `Get`，不能假定未广播。
- `Track(ctx, id, executionClient)` 重启后只用日志中的原 txid 查询，不需重建过期授权/Prepare；完全只读，不因 Found 而修改为成功或因未找到而重播。调用方应对整轮数据库和网络操作设置有界 context；本库不启动后台执行器、轮询或队列。

这些约束只对共享同一数据库日志、遵循上述流程的实例生效。`Reserve` 不锁定链上 nonce/余额，存储层仍将交易字节视为不透明数据：应使用上面的可信构建器完成 ID/签名/授权绑定/有效期校验，再按 ID/Payload 保存，不能直接接收付款请求提供的任意交易。调用者仍须完成合约预检、账户权限、费用资源和风控闸门；最终执行/固化与到账核验尚未实现，**任何日志状态都不能作为付费资源交付凭据**；没有“恰好一次链上付款”的保证。

签名数据可被广播，数据库须最小权限、加密与备份；`Transaction.Payload` 不参与 JSON 序列化，仍禁止自行打印。运行账号不应拥有 DELETE/TRUNCATE 权限，不自动清理过期占用；回滚代码也不能删除历史日志。CI 的 localhost 临时测试库使用 trust 认证，**不是部署配置**。

## 验证

```bash
go mod verify
go vet . ./exact ./exact/postgres
go test -race -count=1 -cover . ./exact ./exact/postgres
go build . ./exact ./exact/postgres
```

PostgreSQL 集成测试要求 `X402_TEST_DATABASE_URL` 指向专用测试库，并授予创建临时 schema 的权限。每个用例使用独立随机 schema，结束后仅清理自己的 schema；未设置时明确跳过数据库测试，不能当作集成验证通过。CI 配置临时 PostgreSQL 16 并实际执行这些测试。

测试覆盖地址、网络、TronWeb 编码/签名对照、授权/费用/时间边界、模糊测试、模拟 JSON-RPC/原生节点，以及真实 PostgreSQL 下的并发与恢复。包括写前已提交 unknown、节点收到后 ACK 丢失、受理后 SQL 失败、只读恢复且不重播。早期存储用例使用不透明夹具，串联用例使用公开 Protobuf 向量；没有真实链上测试。下一步是固化回执、执行及到账核验、官方 SDK 机制接口和 Nile 闭环，再由支付网关引用版本。

## 参考与发布状态

- [TRON 官方地址格式](https://developers.tron.network/docs/account)
- [x402 官方 Go SDK](https://github.com/x402-foundation/x402/tree/main/go)
- [TRON 生态参考实现](https://github.com/BofAI/x402)

使用 [Apache-2.0](LICENSE) 许可证；第三方参考与依赖见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。尚未发布稳定版本，也未通过独立安全审计。
