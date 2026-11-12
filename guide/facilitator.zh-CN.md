# 自建 facilitator

简体中文 | [English](facilitator.md)

本文介绍如何运行 `exact/postgres` 中基于 PostgreSQL 持久化的 facilitator，包括嵌入资源服务端、以 HTTP 服务对外提供、测试网付款示例、官方机制约定，以及保证安全恢复的结算日志。

## 部署方式

| 方式 | 适用场景 | 入口 |
| --- | --- | --- |
| 进程内嵌入 | facilitator 与资源服务端运行在同一进程 | `postgres.NewLocalClient` |
| HTTP 服务 | facilitator 作为独立服务运行 | `postgres.NewHTTPHandler` |
| 已有 facilitator | 已在运营独立的 facilitator | 官方 `HTTPFacilitatorClient` |

`examples/` 下的 HTTP 程序只是可选示例，导入 SDK 并不依赖它们。

### 嵌入自己的应用（推荐）

1. 用 `postgres.New(pool, config.Preflight.Authorization)` 创建 store。管理员须事先应用 schema（见[持久化结算日志](#持久化结算日志)）。
2. 用 `postgres.NewFacilitator(store, config)` 创建机制，它实现了官方 facilitator 机制。
3. 用 `postgres.NewLocalClient(mechanism, 8)` 包装。
4. 把结果传给资源服务端中官方的 `x402.WithFacilitatorClient(client)`。

这种方式不需要 facilitator URL、Bearer 令牌、监听端口或独立服务。数据库连接池和交易签名器由应用自行管理。适配层复用官方 core 以及现有的结算与恢复逻辑，不重复实现协议，也不削弱任何校验。

进程内客户端的行为：

- 只接受 x402 v2，输入总大小上限 64 KiB。
- 拒绝重复或有歧义的键、未知结构字段以及尾随 JSON。
- `maxConcurrent` 允许 1 到 64，传 `0` 时默认为 8。
- 并发已满时立即返回 `ErrLocalClientCapacity`，不会进入无界队列。
- 取消和超时保持标准 `context` 语义。
- 请求不能指定节点、签名器或可信合约。

该并发上限只作用于这个客户端，既不是费用预算，也不是应用层鉴权。

付款失败或仍待确认时，一律不允许交付。恢复只使用原授权和同一份持久化日志。

### 以 HTTP 服务提供

```go
handler, err := postgres.NewHTTPHandler(mechanism, postgres.HTTPConfig{BearerToken: token, MaxConcurrent: 8})
```

该处理器内部注册官方 SDK core，提供三个端点：`GET /supported`、`POST /verify` 和 `POST /settle`。

- 所有端点都要求 Bearer 令牌，令牌为 32 到 256 字节、不含空白的 ASCII 字符。
- JSON 请求体上限 64 KiB。
- 拒绝重复或有歧义的键、未知信封字段、尾随内容以及非 v2 请求。
- 同时执行的校验与结算默认最多 8 个，可设为 1 到 64。
- 超出上限的请求直接返回 `503`，不会签名或广播。

该上限是单进程的并发限制，不是累计费用预算。

### 运行 facilitator 示例服务

[可运行示例](../examples/facilitator/main.go) 把 `NewHTTPHandler` 封装成服务，面向 Linux 和 macOS。它只允许 Nile 和 Shasta，只监听字面 loopback 地址，**拒绝主网和公网绑定**。

配置以下变量后执行 `go run ./examples/facilitator`：

| 变量 | 必填 | 含义 |
| --- | --- | --- |
| `X402_NETWORK` | 否 | 默认 Nile，也可设为 Shasta。 |
| `X402_DATABASE_URL` | 是 | 独立的日志数据库。使用可信的 `search_path` 和最小权限的运行角色，远程连接须启用 TLS。 |
| `X402_EXECUTOR_KEY_FILE` | 是 | 绝对路径，指向服务用户所有、权限为 0400 或 0600 的普通文件。内容为 64 个十六进制字符，末尾最多一个换行。路径最后一级为符号链接时拒绝。 |
| `X402_FACILITATOR_TOKEN` | 是 | 必须与资源服务端一致。通过部署环境注入，不得写入源码、URL 或日志。 |
| `X402_PERMIT2_ADDRESS`、`X402_PROXY_ADDRESS` | 是 | 经独立核验的合约部署地址，不提供默认值。 |
| `X402_PERMIT2_CODE_HASH`、`X402_PROXY_CODE_HASH` | 是 | 可信运行时字节码的 Keccak-256 哈希，格式为 `0x` 加 32 字节。 |
| `X402_JSON_RPC_URL` | 是 | 可信的 HTTPS JSON-RPC 端点。 |
| `X402_RPC_BATCH` | 否 | 默认 `false`。只有字面值 `true` 才会合并独立读取。节点必须支持批量请求，不支持时不会降级。 |
| `X402_NATIVE_NODE_URL` | 是 | 原生节点的 HTTPS 根 URL，必须提供 `wallet` 和 `walletsolidity` 路由。 |
| `X402_GENESIS_BLOCK_ID` | 是 | 经独立核验的完整创世区块 ID。 |
| `X402_NODE_API_KEY` | 否 | 访问上述节点的服务凭据，从不从请求中接收。 |
| `X402_ASSET`、`X402_PAY_TO` | 是 | 固定的代币和收款人。 |
| `X402_MAX_AMOUNT_ATOMIC` | 是 | 单笔金额上限，最小单位的正整数。 |
| `X402_FEE_LIMIT_SUN` | 是 | 单笔交易的正数费用上限，**不是**累计 Gas 预算。 |
| `X402_SETTLEMENT_TIMEOUT` | 否 | 默认 `90s`，允许 1 秒到 3 分钟。原交易生命周期固定为 60 秒。已提交的交易在超时后仍可恢复。 |
| `X402_MAX_CONCURRENT` | 否 | 默认 8。 |
| `X402_FACILITATOR_LISTEN_ADDR` | 否 | 默认 `127.0.0.1:8081`。 |

#### 数据库准备

- 由管理员使用单独的迁移角色，显式应用 [schema.sql](../exact/postgres/schema.sql)。
- 已有的表须按[持久化结算日志](#持久化结算日志)中的说明升级。
- 不要把数据库 URL 或口令放在命令行参数里。
- 运行角色不应拥有 DDL、`DELETE` 或 `TRUNCATE` 权限。

#### 启动时检查什么

启动时会检查配置、数据库连通性，以及当前版本的列是否可读。

启动时**不会**建表或迁移、逐一检查约束、探测链上部署、签名、批准代币授权或提交付款。这不是完整的部署验收，以下各项须自行核验：

- 完整的数据库 schema；
- 执行账户的默认 owner 权限、TRX 余额和能量；
- 代币语义；
- 付款人的 Permit2 授权额度。

#### 对接示例资源服务端

本地联调时，在资源服务端设置以下变量，然后执行 `go run ./examples/server`：

- `X402_FACILITATOR_URL=http://127.0.0.1:8081`
- 相同的 `X402_FACILITATOR_TOKEN`
- `X402_ALLOW_INSECURE_LOCAL_FACILITATOR_HTTP=true`

跨机器部署时，必须由可信的 TLS 入口转发到本机 loopback 监听，并在入口配置限流和累计费用预算。

#### 密钥处理

- 示例只加载显式配置的密钥文件，不会查找或修改已有钱包。
- 私钥只保存在进程内存中，关闭时尽力擦除。这不等同于 HSM 或 MPC 级别的安全。
- 通过 SDK 集成时，可以改用 `FacilitatorConfig.Signer` 提供外部签名器。

#### 响应与关闭

- 响应使用官方格式。
- 结果未知或尚未确认时，绝不报告成功。
- HTTP 失败时保留 `settlement_pending` 原因和原 txid，可用相同载荷恢复。
- 断线或 `503` 不能证明付款失败，禁止生成替代授权。
- 收到 `SIGTERM` 时会等待已接收的请求处理完毕。被中断的工作从同一份日志恢复。不会自动启动后台执行器，也不会自动重新广播。

## 测试网付款示例

[payer 示例](../examples/payer/main.go) 会在测试网上真实付款，必须显式启用，运行于 Linux 和 macOS。它与只做离线握手的 `examples/client` 是两个独立示例。

### 配置

以 [.env.example](../.env.example) 为模板配置：

- 可信的网络、Permit2 地址和代理地址；
- 代币和收款人；
- `X402_MAX_AMOUNT_ATOMIC`、`X402_PAYER_ADDRESS` 和 `X402_PAYER_KEY_FILE`；
- 资源 URL。

模板不会自动加载。切勿把真实的凭据、密钥或授权提交到仓库。

然后准备状态目录：

1. 创建一个**服务用户所有、权限为 0700 的状态目录，且其所有父目录都由你私人控制**。
2. 把它的绝对路径设为 `X402_PAYMENT_STATE_DIR`。
3. 设置 `X402_ENABLE_TESTNET_PAYMENTS=true`。

### 付款与恢复

```bash
go run ./examples/payer pay
# 断线、失败、pending 或进程中断后，使用同一目录和配置恢复，切勿再次付款。
go run ./examples/payer recover
```

`pay` 的执行顺序：

1. 以排他方式持久化付款意图。如果意图已存在，`pay` 拒绝执行。
2. 官方客户端根据 402 报价签署一个授权。
3. 在发送之前保存原始的 `PAYMENT-SIGNATURE` 请求头。

`recover` 先核对原配置与签名是否仍然一致，再原样重发同一个请求头，不会生成新的 nonce。它可能完成一个此前从未提交的授权，因此**不是只读查询**。恢复过程从不加载付款密钥。

每次尝试的记录以及成功响应的内容都保存在私有文件中。标准输出只包含受限的元数据。

### 请求限制

- 只支持对固定 URL 发起 `GET`，网络限 Nile 或 Shasta。
- 默认要求 HTTPS。使用明文 HTTP 须设置 `X402_ALLOW_INSECURE_LOCAL_RESOURCE_HTTP=true`，且只能是字面 loopback IP。
- 拒绝主网、URL 中的凭据、query、fragment 以及重定向。
- 密钥必须是服务用户所有、权限为 0400 或 0600 的普通文件。

### 回执校验

只有当响应为 `200`，且 `PAYMENT-RESPONSE` 回执表示成功并满足以下条件时，示例才报告成功：

- 与配置的网络一致；
- 付款人为配置的付款人，可以是 Base58 格式，也可以是同一地址的 `0x` 签名格式；
- 回执包含金额时，与报价金额一致（x402 中金额字段可省略）；
- txid 非空。

这项校验依赖可信的 facilitator 服务，不是独立的链上证明。

### 恢复规则

> [!WARNING]
> 切勿清空状态目录来“重试”，也切勿用新目录替代结果未知的付款，否则可能重复付款。

如果意图已保存但没有请求头，须人工确认此前是否已签署或发出授权，不能自动重新签名。

示例不会：

- 为钱包充值或批准代币授权；
- 自动重试；
- 控制累计 Gas 预算；
- 保证业务履约去重。

重放资源 `GET` 可能再次执行 handler。

## 官方 facilitator 机制

使用 `postgres.NewFacilitator(store, config)` 创建机制，并注册到官方 SDK：

```go
x402.Newx402Facilitator().Register([]x402.Network{x402.Network(mechanism.Network())}, mechanism)
```

无需自行实现 TRON 验签、交易编码或结算状态机。构造过程不联网、不迁移数据库、不签名、不广播，也不启动 HTTP 或后台服务。

### 配置

所有可信的 `FacilitatorConfig` 值都必须显式提供：

| 字段 | 含义 |
| --- | --- |
| `Preflight` | 授权域、JSON-RPC 端点，以及经独立核验的 Permit2 和代理合约运行时代码哈希。 |
| `Execution` | 同一网络的原生节点根地址和固定的完整创世 ID，必须提供 `wallet` 和 `walletsolidity` 接口。 |
| `Policy`、`MaxAmount` | 固定的网络、单一资产与收款人，以及最小单位的单笔金额上限。请求不能覆盖。 |
| `ExecutorAddress`、`Signer` | 具备默认 owner 权限、负责支付 Gas 的 EOA，以及外部摘要签名器。签名器必须遵守 context，且不得自行广播。 |
| `FeeLimitSUN` | 单笔交易的正数费用上限。 |
| `Lifetime` | 交易生命周期，必须是整毫秒，大于 15 秒且不超过 10 分钟。 |
| `Timeout` | 整轮结算预算，默认 90 秒，最多 3 分钟。 |
| `PollInterval` | 确认轮询间隔，默认 3 秒，范围 1 到 10 秒。 |

### 支持范围

- 只支持 x402 v2 的 `exact` 方案、Permit2 和 `upfront` 流程。
- 报价超时须为 6 到 600 秒。构建新交易时仍要求剩余有效期至少 15 秒。
- 机制声明 `supportedAssetTransferMethods: ["permit2"]`，签名人声明为配置的执行账户。
- 所有部署与网络配置必须与 store 一致。
- 不支持动态多商户路由、其他方案、扩展、多签账户和合约钱包。

### Verify

`Verify` 是对新付款的只读检查，不占用日志、不签名、不广播。`Verify` 通过并不保证之后结算一定成功。

### Settle

`Settle` 不依赖事先调用 `Verify`。

处理新付款时：

1. 检查签名、策略、时间窗口和链上状态。
2. 在日志中占用该授权。
3. 在行锁数据库事务内再次检查状态，然后构建、签名并提交唯一的原交易。
4. 事务提交之后才广播。

预检、广播和最终性等待都在该数据库事务之外进行。多个实例竞争时，失败的一方会对齐获胜方的交易，而不是创建新的付款。

对于已处于 `broadcast_unknown`、`pending` 或 `succeeded` 的授权：

- 请求必须与可信报价要求、签名以及持久化身份一致，随后恢复原交易。
- 不再按当前有效期重新准入，也不再查询已被消费的 nonce。
- `VerifyAuthorizationBinding` 只用于识别历史记录，不能代替新付款的 `VerifyAuthorization` 和 `Preflight.Check` 准入。

### 结算结果

| 结果 | 何时返回 |
| --- | --- |
| `success=true`，附原 txid、network、payer 和 amount | 只有 `ConfirmSettlement` 成功持久化之后。 |
| `settlement_pending`，附原 txid | 预算耗尽、广播结果未知，或核验节点不可用。缺少固化证据时会先在预算内轮询。 |
| `settlement_recovery_required` | 持久化结果不明确。须先读回日志，再做任何操作。 |

无论是 `settlement_pending` 还是 `settlement_recovery_required`，都不允许释放占用、重新签名已保存的交易或创建新的付款。

### 重试与超时

- 官方资源服务端对带原 txid 的 pending 结果**只用相同字节自动重试一次**，不会无限重试。
- 有界等待由机制自身负责。
- 配套的 HTTP 示例已对齐远程、中间件和写超时。反向代理和调用方也必须覆盖同样的预算。
- 中断之后，用相同的付款载荷恢复，切勿重新授权付款。

### upfront 流程

- `upfront` 流程在 handler 执行之前完成结算。如果 handler 随后失败，付款不会自动退款，也不要再次结算。
- 应用需要自行实现履约幂等、查询和补偿。
- 新的 `upfront` 报价需要更新后的客户端。原有不带 `paymentFlow` 的报价和签名仍然兼容。
- 签名摘要不绑定 `paymentFlow`。校验器和官方服务端会比对 accepted 条款与可信报价，因此无法通过剥离该字段来降级流程。

### 外部签名

- 外部签名期间，机制会占用一条数据库连接并持有行锁。应设置较短的签名超时，并据此规划连接池大小。
- 如果进程在签名之后、保存提交之前崩溃，后续尝试可能再次调用签名器。本库从不广播未提交的字节。

> [!WARNING]
> **不保证 HSM 调用恰好一次。**

## 持久化结算日志

结算日志是 facilitator 背后的 PostgreSQL 表。它保证每个授权最多只有一笔签名交易、只广播一次，并且每次恢复都复用这笔原交易。

### 初始化

- `exact/postgres.New` 接受调用方管理的 `pgxpool.Pool` 和可信的 `exact.Config`。
- 在专用、可信的 `search_path` 下显式应用 [schema.sql](../exact/postgres/schema.sql)，同样的 SQL 也可通过 `SchemaSQL` 获取。
- 构造过程从不执行 DDL。
- 其他包不需要数据库。日志不依赖任何支付网关或商户账本。

### 升级已有日志

> [!WARNING]
> **已有日志库必须在新版本启动前完成升级**。仅重新执行 `SchemaSQL` 不会升级已有的表。

1. 备份数据库，停止所有旧版本的写入。
2. 在同一个可信 `search_path` 下显式执行 [upgrade_confirmation.sql](../exact/postgres/upgrade_confirmation.sql)，同样的 SQL 也可通过 `UpgradeConfirmationSQL` 获取。
3. 启动新版本。

该脚本在事务中运行，新增确认相关的列和约束。它可以重复执行，并保留历史交易和状态。执行失败时，须回滚未完成的事务。

运行账号不需要 DDL 权限。不要让不认识成功状态的旧版本与新版本混跑。不要通过删除列、清除确认结果或降级状态来回滚。

### 状态

```
reserved ──SaveTransaction──▶ prepared ──BeginBroadcast──▶ broadcast_unknown ──RecordAccepted──▶ pending
                                                                  │                                 │
                                                                  └────────ConfirmSettlement────────┴──▶ succeeded
```

| 状态 | 含义 |
| --- | --- |
| `reserved` | 授权已占用，尚未保存交易。 |
| `prepared` | 签名交易及其 ID 已保存，尚未广播。 |
| `broadcast_unknown` | 已授予广播许可，交易可能已到达节点，也可能没有。 |
| `pending` | 节点已受理交易，但这不代表成功。 |
| `succeeded` | 固化交易和精确到账已由可信节点核验并持久化。 |

记录一旦进入 `broadcast_unknown`、`pending` 或 `succeeded`，交易就不会再次发送。

### Store 操作

**`Reserve`** 先离线验证授权，再按网络、Permit2、付款人和规范化 nonce 的组合进行占用。

- 相同授权返回原记录。
- 同一组合下摘要不同则冲突。
- 不同代理共享同一个 Permit2 nonce 空间。

**`SaveTransaction`** 原子地持久化唯一的交易 ID 和签名数据。

- 并发写入时只有一份不同结果能够胜出，相同内容的写入是幂等的。
- 已保存的数据不能被替换。
- 同一网络下的同一 txid 不能绑定到其他授权。

**`BeginBroadcast`** 先提交 `broadcast_unknown`，**然后**才返回唯一的广播许可和持久化的交易。

- 切勿广播尚未保存的本地数据。
- 只有该调用成功后才能广播。

**`RecordAccepted`** 将节点受理记录为 `pending`。仅凭受理绝不会标记为成功。

- 如果记录已是 `succeeded`，txid 和摘要一致的回调会保留成功状态和确认时间。
- 遇到超时、进程退出、查不到交易或数据库提交结果不明时，调用 `Get` 并追踪原 txid。切勿重新签名、替换交易或创建新付款。
- 在真正提交之前崩溃，也可能让记录停留在 `broadcast_unknown`。目前没有实现自动重新广播或释放占用。

**`Submit(ctx, id, prepared, executionClient, now)`** 是推荐的广播方式。它读取原始字节并校验，通过 compare-and-swap 将记录改为 `broadcast_unknown`，只广播这份持久化快照，然后记录受理。

- 并发或重复调用只能获得一次许可。
- 处于 `broadcast_unknown`、`pending` 或 `succeeded` 的记录不会再次发送。
- 节点已受理但确认结果无法持久化时，记录保持 `broadcast_unknown` 并保留原 txid。
- 如果 SQL 提交本身结果不明，须调用 `Get`，切勿假定许可尚未使用。

**`Track(ctx, id, executionClient)`** 使用日志中的 txid 进行只读恢复。

- 不需要已过期的授权，也不需要 `Prepare` 对象。
- 不会根据查询结果标记成功或重新广播。
- 调用方须用 context 约束整轮数据库和网络操作。
- 不会启动后台执行器、轮询或队列。

**`store.CheckSettlement(ctx, id, executorAddress, executionClient, now)`** 对记录进行审计，不做任何修改。

- 从处于 `broadcast_unknown`、`pending` 或 `succeeded` 的原记录中恢复准备绑定。
- 先核对可信合约、执行账户和原授权身份，再核验固化交易和精确到账。
- 不需要内存中的准备对象，过期后也可以核验。
- 从不修改状态或时间戳，也从不广播。
- 无效或不匹配的旧交易字节会在读取节点之前就被拒绝。
- 之后的审计即使缺少证据或出错，也不会自动撤销已持久化的成功。

**`store.ConfirmSettlement(ctx, id, executorAddress, executionClient, now)`** 是记录成功的唯一途径。

- 先执行与 `CheckSettlement` 相同的核验，再以原授权、txid 和签名字节为条件，通过 compare-and-swap 原子地将记录从 `broadcast_unknown` 或 `pending` 改为 `succeeded`。
- `Record.Confirmation` 保存区块高度、区块 ID 和数据库确认时间。
- 没有任何公开接口可以直接传入成功标志或调用方提供的回执来修改状态。
- 并发写入相同结果是幂等的，冲突的区块绝不会覆盖已有确认。
- 证据尚未确认时返回 `ErrSettlementUnconfirmed`。
- 出现任何错误（包括提交响应丢失）后，须先调用 `Get` 复核，才能认定成功。错误绝不意味着可以重新付款。

对于已处于 `succeeded` 的记录：

- `ConfirmSettlement` 先恢复并核对原绑定和可信配置，再返回已持久化的结果，不读取节点，也不修改时间戳。需要用当前节点重新审计时，显式调用 `CheckSettlement`。
- 重新保存相同字节以及迟到的受理回调都会保留成功状态。
- `BeginBroadcast` 和 `Submit` 不会再授予广播许可。

SQL、恢复、RPC 和提交各步骤须由同一个调用方 context 约束。

### 保证与限制

- 上述保证只对共享同一可信数据库、并遵循本流程的协作实例有效。
- 占用不会锁定链上 nonce 或资金。
- `SaveTransaction` 把交易字节视为不透明数据。须使用可信构建器绑定 ID、签名、授权和有效期，切勿接受客户端提供的任意交易字节。
- `succeeded` 保存的是可信节点的核验结果，不是独立的链上证明。数据库写权限仍然是信任边界。
- 合约预检、账户权限检查、费用资源检查和风控闸门仍然必不可少。

> [!WARNING]
> **成功记录不绑定 URL 或业务请求，也不授权重复交付资源**。本库既不保证链上付款恰好一次，也不保证交付恰好一次。HTTP 鉴权、业务请求与报价的关联以及履约幂等，均由应用负责。

### 保护日志数据

- 签名数据可以被直接广播。须使用最小权限的数据库访问，并做好加密和备份。
- `Transaction.Payload` 不参与 JSON 序列化，切勿自行写入日志。
- 运行账号不应拥有 `DELETE` 或 `TRUNCATE` 权限。
- 不要自动清理过期的占用，回滚代码时也不要删除历史记录。
- CI 中的 localhost 临时数据库使用 trust 认证，**不是部署配置**。

## 运营方职责

- 单笔金额上限和费用上限都不是累计代付预算。
- 公开端点仍需身份认证、限流和费用预算。
- 须独立核验合约部署、代币语义、Gas 资源和账户权限。
