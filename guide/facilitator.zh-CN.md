# 自建 facilitator

简体中文 | [English](facilitator.md)

运行基于 PostgreSQL 持久化的 facilitator：进程内嵌入、HTTP 服务、测试网付款示例、结算日志以及官方机制约定。

## 部署方式

### 优先按库嵌入自己的应用

`postgres.NewFacilitator(store, config)`实现官方机制；再调用`postgres.NewLocalClient(mechanism, 8)`，即可直接作为官方`x402.WithFacilitatorClient(client)`传给自己的资源服务端，无需facilitator URL、Bearer令牌、监听端口或独立服务。`store`由`postgres.New(pool, config.Preflight.Authorization)`构造，`pool`和费用签名器仍由应用管理，管理员事先显式应用schema。此适配器复用官方core及原结算/恢复逻辑，不复制协议或降低验证。

进程内入口只接受v2、有界64KiB总载荷，拒绝重复/歧义键、未知结构字段及尾随JSON；`maxConcurrent=0`默认8，允许1~64，满载返回`ErrLocalClientCapacity`，不排无界队列。该上限只覆盖这个client，不是累计费用预算或应用鉴权。取消/超时保留context语义，失败/待确认不交付，恢复只认原授权与同一持久化日志；不允许由请求选择节点、签名器或可信合约。已有独立facilitator时，仍使用官方`HTTPFacilitatorClient`对接即可。

下方HTTP程序是可选示例，不是普通导包接入的前置条件。

### 真实付款客户端与原请求恢复

[payer 示例](../examples/payer/main.go) 是 Linux/macOS 的显式测试网客户端，与只做离线握手的 `examples/client` 分开。使用 [.env.example](../.env.example) 配置可信网络/Permit2/代理、资产/收款人、`X402_MAX_AMOUNT_ATOMIC`、`X402_PAYER_ADDRESS`、`X402_PAYER_KEY_FILE` 和资源 URL；模板不自动加载，不应把真实口令、密钥或授权提交到仓库。

预先建立当前用户独占的 **0700 状态目录及私人控制的父目录**，将绝对路径设为 `X402_PAYMENT_STATE_DIR`，再显式设置 `X402_ENABLE_TESTNET_PAYMENTS=true`：

```bash
go run ./examples/payer pay
# 断连、失败、pending或进程中断后，只用同一目录和配置恢复；不要再执行pay。
go run ./examples/payer recover
```

只支持固定 URL 的 GET、Nile/Shasta；默认要求 HTTPS。明文须显式 `X402_ALLOW_INSECURE_LOCAL_RESOURCE_HTTP=true` 且仅限字面 loopback IP。拒绝主网、URL 凭据/query/fragment和重定向。付款密钥为当前用户所有的0400/0600普通文件，恢复不读取密钥。

`pay` 先排他保存付款意图，再由官方客户端根据402报价签署唯一授权，并在发送前保存原 `PAYMENT-SIGNATURE`；已有意图即拒绝新的pay。`recover` 验证原配置/签名绑定，仅重发同一header，不生成nonce；它可能完成原先尚未提交的授权，**不是纯只读查单**。每次尝试及成功资源内容保存在私有目录，只输出受限元数据。HTTP回执必须匹配网络、付款人、金额和非空txid，仍依赖可信服务，不是独立链上证明。

状态目录不能清空后“重试”，尤其不能用新目录替代未知付款。若意图已保存但没有header，需人工确认此前是否签署/发出，不能自动补签。示例不领水、不approve、不自动重试、不保证累计费用预算或业务履约幂等；这些由接入方控制。普通资源GET重放可能再次执行handler。

可复用 `postgres.NewHTTPHandler(mechanism, postgres.HTTPConfig{BearerToken: token, MaxConcurrent: 8})`，内部注册官方 SDK 并提供 `GET /supported`、`POST /verify`、`POST /settle`。令牌须为32~256字节无空白 ASCII，所有端点都认证；JSON限64KiB，拒绝重复/歧义键、未知信封字段、尾随内容和非v2请求。默认最多8个同时执行的预检/结算（可设1~64），满载直接503且不签名/广播；这是单进程并发上限，不是累计费用预算。

[可运行示例](../examples/facilitator/main.go) 面向 Linux/macOS，只允许 Nile/Shasta 和字面 loopback IP 监听，**拒绝主网和公网绑定**。配置以下变量后执行 `go run ./examples/facilitator`：

| 变量 | 含义 |
| --- | --- |
| `X402_NETWORK` | 默认 Nile；也可显式配置 Shasta |
| `X402_DATABASE_URL` | 必填，独立日志库；可信 `search_path`、最小运行权限，远程库使用 TLS |
| `X402_EXECUTOR_KEY_FILE` | 必填，绝对路径；服务用户拥有的0400/0600普通文件，64个hex字符（可带一个换行），拒绝末级符号链接 |
| `X402_FACILITATOR_TOKEN` | 必填，与资源服务端一致；通过部署环境注入，不写进源码、URL或日志 |
| `X402_PERMIT2_ADDRESS` / `X402_PROXY_ADDRESS` | 必填，独立核验的可信部署，不提供默认地址 |
| `X402_PERMIT2_CODE_HASH` / `X402_PROXY_CODE_HASH` | 必填，可信 runtime bytecode 的 `0x` + 32字节 Keccak-256 |
| `X402_JSON_RPC_URL` | 必填，可信 HTTPS JSON-RPC 端点 |
| `X402_RPC_BATCH` | 可选，默认`false`；仅显式`true`合并独立读取，节点必须支持JSON-RPC batch，失败不降级 |
| `X402_NATIVE_NODE_URL` / `X402_GENESIS_BLOCK_ID` | 必填，HTTPS原生节点根URL及独立核验的完整创世块ID；节点须提供 wallet/walletsolidity |
| `X402_NODE_API_KEY` | 可选，上述节点的服务凭据，不从请求接收 |
| `X402_ASSET` / `X402_PAY_TO` / `X402_MAX_AMOUNT_ATOMIC` | 必填，固定代币/收款人/单笔金额上限；金额为最小单位正整数 |
| `X402_FEE_LIMIT_SUN` | 必填，单笔正数费用上限，**不是**累计Gas预算 |
| `X402_SETTLEMENT_TIMEOUT` | 默认 `90s`，允许1秒~3分钟；原交易生命周期固定60秒，已提交原交易仍可继续等待/恢复 |
| `X402_MAX_CONCURRENT` / `X402_FACILITATOR_LISTEN_ADDR` | 默认8 / `127.0.0.1:8081` |

首次部署由管理员用单独迁移账号显式应用 [schema.sql](../exact/postgres/schema.sql)，已有旧表须按下文“持久化结算日志”升级；不把数据库URL/口令放进命令行参数。运行角色不应有 DDL、DELETE、TRUNCATE 权限。启动只检查配置、数据库连通性和当前版本列的可读性，**不建表、不迁移、不扫描全部约束、不探测链上部署、不签名、不批准代币授权、不发交易**。这不是完整部署验收；管理员仍须核对完整 schema、费用付款方默认 owner 权限与TRX/能量、代币语义和付款人 Permit2 allowance。

本地资源服务端可设置 `X402_FACILITATOR_URL=http://127.0.0.1:8081`、同一令牌和 `X402_ALLOW_INSECURE_LOCAL_FACILITATOR_HTTP=true` 后启动 `go run ./examples/server`；跨机器部署必须由可信 TLS 入口转发到本机监听，并配置入口限流/累计费用预算。私钥仅在内存使用，关闭时尽力擦除，不等于 HSM/MPC；SDK 可用外部 `FacilitatorConfig.Signer`。示例只加载显式指定的密钥文件，不自动查找或修改已有钱包。

接口返回官方响应结构；未知/待确认仍为 `success=false`，HTTP错误保留 `settlement_pending` 和原 txid，官方客户端可原样恢复。断线/503不代表原付款失败，禁止重新生成付款授权。SIGTERM优雅关闭等待已接收请求结束，强制中断后依靠同一数据库日志恢复，不自动启动后台执行器或重播。

先结算意味着业务 handler 随后失败时不会自动退款，也不应再次结算；应用需设计履约幂等、查询和补偿。新服务端的 upfront 报价需要本次新版客户端；原有不带 paymentFlow 的报价和签名仍兼容。授权摘要不绑定 paymentFlow，校验器与官方服务端会核对可信报价和 accepted 一致，不能通过剥离该字段降级流程。

## 持久化结算日志

`exact/postgres.New` 接受调用方管理的 `pgxpool.Pool` 和可信 `exact.Config`。先在专用、可信的 `search_path` 下显式应用 [schema.sql](../exact/postgres/schema.sql)（也可通过 `SchemaSQL` 获取），构造器不自动执行 DDL。未导入该包的工具/验签消费者不需要数据库；日志也不依赖支付网关或商户账本。

**已有日志库必须先升级：**备份并停止旧版写入，在同一可信 `search_path` 下显式执行 [upgrade_confirmation.sql](../exact/postgres/upgrade_confirmation.sql)（`UpgradeConfirmationSQL`），然后启动新版。脚本以事务新增确认列并扩展约束，可重复执行、不改历史交易或状态；失败须回滚未完成事务。仅重跑 `SchemaSQL` 不会升级旧表。运行账号不需要 DDL 权限；不要混跑不认识成功终态的旧版本，也不要通过删除列、清确认结果或降级状态来回滚。

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

## 官方 facilitator 机制

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
- 官方资源服务端仅对带原 txid 的 pending **原样自动重试一次**，不是无限轮询。机制自身负责有界等待，配套 HTTP 示例已匹配远程/中间件/写超时；反向代理和调用方也需覆盖预算。中断后仍须用相同付款载荷恢复，不能重新授权付款。

外部签名期间占用一条数据库连接及行锁，应配置短签名超时和合理连接池。崩溃发生在签名后、保存提交前时，本库从未广播那些未提交字节，后续可能再次调用签名器；**不保证 HSM 调用恰好一次**。所有实例必须共享同一日志并遵循本流程。单笔金额/费用上限不是累计代付预算，公开端点还需身份认证、限流和费用预算；运营方仍须独立核验部署、资产语义、费用资源和账户权限。
