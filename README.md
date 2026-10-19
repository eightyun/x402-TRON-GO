# x402-tron-go

简体中文 | [English](README.en.md)

面向 x402 的独立 TRON Go 适配 SDK，目标是让客户端/Agent、API 服务端和 facilitator 直接使用 TRON 支付，不必自行实现链适配，也不依赖我们的支付网关。当前已有官方三端机制、facilitator HTTP 启动示例，以及授权预检、交易构建/广播、固化精确到账核验和持久化恢复。**已完成真实 Nile HTTP 付款、成功终态及待确认进程中断后的跨进程恢复；仍是未审计的测试网开发版本，不可用于承载真实资金**。本项目不隶属于 x402 Foundation 或 TRON。

**这是可直接导包的 SDK，不是需要部署的支付平台。** 普通应用导入机制并注册到官方 x402 SDK 即可接入；不要求运行本仓库的 `examples/*` 服务、Docker 或支付网关。`examples/` 和二进制打包只作可选演示/验收，不是库的运行依赖。签名器、可信网络/合约、资产/收款人/限额仍须显式配置，导包不会自动寻找钱包或发起交易。

## 安装 SDK

在自己的 Go 1.26+ 项目中执行：

```bash
go get github.com/eightyun/x402-tron-go/exact@main

# 仅自建持久化 facilitator 才需要；普通付款端/资源端不需要它。
go get github.com/eightyun/x402-tron-go/exact/postgres@main
```

当前尚无稳定版本tag；`@main` 会解析成写入应用`go.mod/go.sum`的固定伪版本，不是在运行时追踪分支。需要复现时将`main`换成审核过的完整commit。应安装实际导入的子包：只`go get`标准库根工具包，不一定补齐`exact`所需的依赖校验。

| 导入路径 | 职责 | 接入方需要什么 |
| --- | --- | --- |
| 根包 `github.com/eightyun/x402-tron-go` | 地址、网络工具 | 仅Go标准库 |
| `.../exact` | 官方客户端/资源端机制、验签与链结算组件 | 显式策略/签名器；客户端和资源端不需要数据库 |
| `.../exact/postgres` | 可选的持久化facilitator | 自管pgx连接池、可信节点、交易签名器；不强制独立服务 |

[客户端/资源端可运行API示例](exact/example_test.go)和[进程内facilitator示例](exact/postgres/example_test.go)会随测试执行，不要求使用`examples`里的main程序。官方职责划分见[x402客户端指南](https://docs.x402.org/getting-started/quickstart-for-buyers)与[facilitator说明](https://docs.x402.org/core-concepts/facilitator)；公共facilitator是否支持TRON必须另行核对，不能把EVM/Solana端点直接当作TRON结算端。

仓库外消费回归在独立模块[integration/consumer](integration/consumer/go.mod)：固定公开伪版本、没有`replace`，覆盖Nile/Shasta离线签名/报价/限额、三端官方接口及构造不签名/不连接数据库。可在该目录执行`GOWORK=off go test -race -count=1 .`。CI另复制到临时目录并仅在那里将依赖替换为当前PR源码，验证未发布改动的兼容性；这与实际从公网安装固定版本是不同的证据，不把本地替换当作公网安装通过。

## 开箱即用进度

| 角色 | 当前状态 |
| --- | --- |
| 客户端 / Agent | 已实现官方 `SchemeNetworkClient`；配置付款策略和签名器，由官方 HTTP 客户端处理 402 与签名重试 |
| API 资源服务端 | 已实现官方 `SchemeNetworkServer`、Permit2 报价与 `upfront` 中间件示例；仍需可信 facilitator，不能仅验签就发货 |
| 自建 facilitator | 已实现官方机制和带鉴权/并发上限的 HTTP 示例；真实Nile付款、待确认SIGKILL原交易恢复、解包三程序实付及重启恢复已验证 |

普通客户端不需要 PostgreSQL，不加载支付网关的商户/账本模块。数据库只是自建 facilitator 可选的持久化实现。网络、资产与收款策略、钱包签名、Gas 和必要的 Permit2 代币授权仍是接入前置条件；不自动授权或隐式上链。

**互操作边界：** 已用实际发布的 `@bankofai/x402-tron@1.2.0` / `@bankofai/x402-core@1.1.1` 完成离线双向授权测试，但该版本只配置旧十六进制网络标识（例如 Nile 为 `tron:0xcd8690dc`），不能直接处理本项目默认的十进制报价。Go 机制接受这些已知别名，官方 core 的注册键仍须与报价一致；这不是默认资源示例与该版本完整 HTTP 互通的证明。详见下方验证范围；未改写上游包或放宽签名校验来隐藏差异。

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
| `X402_FACILITATOR_TOKEN` | 本项目自建服务必填；通过官方 `AuthProvider` 给三个端点发送 Bearer，第三方无需认证时可不设 |
| `X402_ALLOW_INSECURE_LOCAL_FACILITATOR_HTTP` | 默认关闭；仅显式 `true` 允许明文连接字面 loopback IP，不接受 `localhost` 或公网 HTTP |
| `X402_LISTEN_ADDR` | 默认 `127.0.0.1:8080` |

示例启动前显式 `Initialize` 并构建报价，配置/能力校验失败就退出，不忽略错误继续监听。提供 `GET /resource`，未付款返回 402；HEAD 被拒绝，不能利用 Go GET 路由的 HEAD 匹配绕过。配置了有界启动/结算/HTTP 超时、禁上游重定向、请求头上限和优雅关闭。对外部署应加 TLS 入口及适当鉴权；第三方 facilitator 认证应使用官方 `AuthProvider`，不要把 token 拼进 URL。

远程调用预算200秒，覆盖 facilitator 最多180秒的机制预算；中间件410秒、HTTP写超时420秒，允许官方对携带原 txid 的 pending 原样重试一次。启动能力发现仍限30秒；反向代理和调用方也需匹配预算。不要把超时解释成付款失败或创建新授权。

下文自建 HTTP 示例仍需独立准备/核验合约和节点，不默认使用公共 facilitator 地址。自动化 HTTP 集成测试使用模拟链状态/回执；实链程序、打包示例的独立进程验收另记如下，**均不等于生产验收**。

### Nile 实链验证（2026-10-18）

核心版本 `39b83a63fab27715306cf6fb273a9dd7184195e5`、Go 1.26.5、官方 Go SDK v2.28.0，使用下文已复现的 Permit2/代理、Nile 测试 USDT `TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf`、独立付款/费用/收款地址与专用 PostgreSQL。一次性本地接入程序调用公开 SDK 和官方 HTTP 中间件，两个服务为 loopback TLS，节点与链上回执真实；没有修改核心实现或放宽预检。

- 限额授权 0.003 测试 USDT 后，由真实 402 报价生成一份 0.001 测试 USDT 授权，完整模拟、保存唯一签名交易、单次广播、固化核验并落库后才返回 HTTP 200 付费资源。
- 结算 [135dfb80…23efe5](https://nile.tronscan.org/#/transaction/135dfb80322290839444d93864f8190ae16cb0aadb1b9d9621110f96b823efe5)，固化块 `71933724`；核对唯一精确 Transfer 与双方余额变化。授权手续费 0、结算手续费 7.237 测试 TRX，仅代表本次实际费用。
- 原进程正常结束后，新进程重放同一授权，从已有 `succeeded` 记录返回同一 txid；新增授权签名、交易签名和广播均为 0，原签名字节、余额及费用不变。**这是成功终态的跨进程恢复，不是待确认期间强制中断的实链演练**。

第二次实付验证了真实待确认中断：结算 [3a339895…18873a](https://nile.tronscan.org/#/transaction/3a339895a1c7f3e66370e33e8bc71c729ff287c826dcf82129fa6ffd7318873a) 在日志为 `pending`、固化主体/回执均不存在且未交付时，执行进程被 SIGSTOP 后 SIGKILL 并确认退出。新进程不加载私钥，复用原header、同一数据库和603字节原签名交易，固化于块 `71934525` 后才交付；新增授权签名、交易签名、广播均为0。第一笔全部记录未变，双方余额精确变化0.001测试USDT，第二笔费5.737测试TRX，两笔累计12.974测试TRX。

第三次使用干净提交`3f1beac5d027598a30e1a03b2b2503880c7d06c5`实际归档/解包后的三个程序独立启动，复用同一专用日志库和剩余有限授权，启用`X402_RPC_BATCH=true`。结算[ea6149a0…1f9249d](https://nile.tronscan.org/#/transaction/ea6149a03be449d61d31770edfba94c2d2690299d6c688a004b5a51431f9249d)固化于块`71935578`后返回HTTP200；服务端与facilitator均停止并以新进程重启，payer无需私钥，用原header恢复同一交易，原603字节摘要及前两笔记录完全不变。运行数据库角色没有DDL/DELETE/TRUNCATE权限，结束后禁用登录，服务全部停止。独立只读审计核对唯一精确Transfer、三笔累计0.003测试USDT、手续费18.711测试TRX、allowance为0；没有追加授权。

这完成了v0.1测试网候选范围的Nile实付、中断恢复和实际打包三端接入。它使用已有数据库/已核验合约和本机loopback服务，不是空白机器安装、独立服务器TLS运维或生产验收；公共RPC仍可能因链头变化拒绝新付款。Shasta/主网、上游SDK端到端实付、长时稳定性及独立安全审计仍未验证。私钥、原始授权和本机运维材料不公开，不提供可照搬的生产资金配置。候选包可自行构建；尚未创建稳定版本tag或GitHub release。

## 自建 facilitator HTTP 示例

### 优先按库嵌入自己的应用

`postgres.NewFacilitator(store, config)`实现官方机制；再调用`postgres.NewLocalClient(mechanism, 8)`，即可直接作为官方`x402.WithFacilitatorClient(client)`传给自己的资源服务端，无需facilitator URL、Bearer令牌、监听端口或独立服务。`store`由`postgres.New(pool, config.Preflight.Authorization)`构造，`pool`和费用签名器仍由应用管理，管理员事先显式应用schema。此适配器复用官方core及原结算/恢复逻辑，不复制协议或降低验证。

进程内入口只接受v2、有界64KiB总载荷，拒绝重复/歧义键、未知结构字段及尾随JSON；`maxConcurrent=0`默认8，允许1~64，满载返回`ErrLocalClientCapacity`，不排无界队列。该上限只覆盖这个client，不是累计费用预算或应用鉴权。取消/超时保留context语义，失败/待确认不交付，恢复只认原授权与同一持久化日志；不允许由请求选择节点、签名器或可信合约。已有独立facilitator时，仍使用官方`HTTPFacilitatorClient`对接即可。

下方HTTP程序是可选示例，不是普通导包接入的前置条件。

### 真实付款客户端与原请求恢复

[payer 示例](examples/payer/main.go) 是 Linux/macOS 的显式测试网客户端，与只做离线握手的 `examples/client` 分开。使用 [.env.example](.env.example) 配置可信网络/Permit2/代理、资产/收款人、`X402_MAX_AMOUNT_ATOMIC`、`X402_PAYER_ADDRESS`、`X402_PAYER_KEY_FILE` 和资源 URL；模板不自动加载，不应把真实口令、密钥或授权提交到仓库。

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

[可运行示例](examples/facilitator/main.go) 面向 Linux/macOS，只允许 Nile/Shasta 和字面 loopback IP 监听，**拒绝主网和公网绑定**。配置以下变量后执行 `go run ./examples/facilitator`：

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

首次部署由管理员用单独迁移账号显式应用 [schema.sql](exact/postgres/schema.sql)，已有旧表须按前文升级；不把数据库URL/口令放进命令行参数。运行角色不应有 DDL、DELETE、TRUNCATE 权限。启动只检查配置、数据库连通性和当前版本列的可读性，**不建表、不迁移、不扫描全部约束、不探测链上部署、不签名、不批准代币授权、不发交易**。这不是完整部署验收；管理员仍须核对完整 schema、费用付款方默认 owner 权限与TRX/能量、代币语义和付款人 Permit2 allowance。

本地资源服务端可设置 `X402_FACILITATOR_URL=http://127.0.0.1:8081`、同一令牌和 `X402_ALLOW_INSECURE_LOCAL_FACILITATOR_HTTP=true` 后启动 `go run ./examples/server`；跨机器部署必须由可信 TLS 入口转发到本机监听，并配置入口限流/累计费用预算。私钥仅在内存使用，关闭时尽力擦除，不等于 HSM/MPC；SDK 可用外部 `FacilitatorConfig.Signer`。示例只加载显式指定的密钥文件，不自动查找或修改已有钱包。

接口返回官方响应结构；未知/待确认仍为 `success=false`，HTTP错误保留 `settlement_pending` 和原 txid，官方客户端可原样恢复。断线/503不代表原付款失败，禁止重新生成付款授权。SIGTERM优雅关闭等待已接收请求结束，强制中断后依靠同一数据库日志恢复，不自动启动后台执行器或重播。

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

可运行 [部署检查示例](examples/deployment/main.go)：显式设置下列环境变量，再执行 `go run ./examples/deployment`。

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

### 已复现的 Nile 合约构建

2026-10-18 使用下列固定源码和编译器进行本地构建，仅按编译器 `immutableReferences` 链接已知构造参数；结果与 Nile `eth_getCode` **全字节一致，包含元数据**。不是仅比较 ABI、getter、长度或移除元数据后的代码，也不是直接采信节点哈希。

| 项目 | Permit2 | exact 代理 |
| --- | --- | --- |
| 地址 | `TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h` | `TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F` |
| 固定源码 | [SUN Permit2 67b1f91](https://github.com/sun-protocol/sunswap-permit2/tree/67b1f91662424416e50d05d734f9fd3856039cc5)（含仓库内 solmate） | [BofAI contracts 82aa2b2](https://github.com/BofAI/x402-contracts/tree/82aa2b2b761ebe61f08b4e6ae2558846c67ded47)，OpenZeppelin 5.6.1 |
| 编译器 | solc `0.8.17+commit.8df45f5f` | solc `0.8.25+commit.b61c2a91` |
| 参数 | optimizer 999999、viaIR、London | optimizer 999999、Cancun |
| runtime 字节数 | 9193 | 3593 |
| runtime Keccak-256 | `0xfdd5ad3c59a61e0d232cc5ec5fe684bbe3083990c0873441a6c928bb452a4705` | `0x45b702594297f3ce7ea10f4ff622608653dd3ce289445269686607ac1d5f626d` |

两者均使用 `metadata.useLiteralContent=true`，对应 [部署插件的设置](https://github.com/sun-protocol/sunhat/blob/2efcfb61092024c69fb86f0568edc4be5c7798b7/src/index.ts#L326)。Permit2 的 immutable 按 Nile chain ID 和其地址计算签名域；代理链接上述 Permit2 地址。**使用的是表中可复现的标准 solc 构建，不是同版本号的 TRON fork 编译器**；后者生成的代理代码不匹配，不能混用。

编译器 SHA-256：0.8.17 为 `617828e63be485c7cc2dbcbdd5a22b582b40fafaa41016ad595637b83c90656c`，0.8.25 为 `f8c9554471ff2db3843167dffb7a503293b5dc728c8305b044ef9fd37d626ca7`，对应 [Solidity 官方编译器索引](https://github.com/ethereum/solc-bin/blob/gh-pages/wasm/list.json)。这些是显式核验的参考配置，**不会自动填入 SDK 或示例**；代码复现本身不是主网配置、安全审计或付款证明。链头漂移等检查仍可能拒绝一轮预检，不能跳过；单笔实付证据和未验证范围见前文。

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

这些请求已用于上述单笔真实 Nile 结算，自动化测试仍使用模拟节点。节点受理、FullNode 主体、执行回执和固化付款事件是不同证据层次；前两个接口不判成功，需使用下面的只读交叉核验，也不提供恶意节点的状态证明。

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

恢复后必须将 `prepared.Authorization()` 与原日志的可信授权标识完全比较，再调用只读固化核验。仅接受本包的单一合约调用、单一 EOA 签名和固定规范编码，其他合法 TRON 交易也可能拒绝；不是通用历史交易导入器。下面的 `ConfirmSettlement` 串联恢复、核验及成功持久化，facilitator 及 HTTP 示例均使用该流程；单笔实付不覆盖所有中断窗口，不能把模拟测试当作上线验收。

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
- 官方资源服务端仅对带原 txid 的 pending **原样自动重试一次**，不是无限轮询。机制自身负责有界等待，配套 HTTP 示例已匹配远程/中间件/写超时；反向代理和调用方也需覆盖预算。中断后仍须用相同付款载荷恢复，不能重新授权付款。

外部签名期间占用一条数据库连接及行锁，应配置短签名超时和合理连接池。崩溃发生在签名后、保存提交前时，本库从未广播那些未提交字节，后续可能再次调用签名器；**不保证 HSM 调用恰好一次**。所有实例必须共享同一日志并遵循本流程。单笔金额/费用上限不是累计代付预算，公开端点还需身份认证、限流和费用预算；运营方仍须独立核验部署、资产语义、费用资源和账户权限。

## 验证

```bash
go mod verify
go vet . ./exact ./exact/postgres ./examples/client ./examples/server ./examples/facilitator
go test -race -count=1 -cover . ./exact ./exact/postgres ./examples/client ./examples/server ./examples/facilitator
go build . ./exact ./exact/postgres ./examples/client ./examples/server ./examples/facilitator
```

PostgreSQL 集成测试要求 `X402_TEST_DATABASE_URL` 指向专用测试库，并授予创建临时 schema 的权限。每个用例使用独立随机 schema，结束后仅清理自己的 schema；未设置时明确跳过数据库测试，不能当作集成验证通过。CI 配置临时 PostgreSQL 16 并实际执行这些测试。

### 完整 HTTP 付款与原交易恢复

设置专用测试数据库后，可单独运行[三端集成测试](exact/postgres/http_roundtrip_integration_test.go)：

```bash
go test -race -count=1 ./exact/postgres -run '^TestHTTPAutomaticPaymentRoundTripAndOriginalRecovery$'
```

官方 HTTP 客户端从实际 402 报价调用 `exact.NewClient` 生成随机 nonce/真实签名，再经测试 TLS 资源服务和鉴权 facilitator 完成预检、完整模拟、日志保存、合成节点广播/固化核验、成功落库后交付。覆盖模拟拒绝零占用/付款/交付、超额报价签名前拒绝，以及 pending 不交付、原授权恢复保持唯一交易、成功后节点不可用仍从日志恢复。失败也可能包含 `PAYMENT-RESPONSE`，必须检查 `success`、错误原因及原 txid，不能以响应头存在判成功。

该自动化测试的付款人密钥仅在测试内随机生成；不会加载已有钱包或发起实链交易，与前述人工执行的 Nile 验证分开。恢复原请求不创建新授权；**防重复付款不等于防重复履约**，资源请求重放可能再次执行 handler，业务交付幂等仍由接入方负责。这不替代示例从零部署、实链付款或进程中断恢复验收。

### 上游 TRON SDK 双向互操作

[互操作测试](interop/bankofai/interop.test.mjs) 使用上游实际发布包的公开 core/client/facilitator，而非复制其算法或只比较JSON字段。Go通过本项目公开客户端/官方core生成授权，反向由上游core/facilitator验证；签名均为真实密码学校验。Nile/Shasta覆盖旧版extra和upfront、签名/金额/收款人/代理/nonce/时效/网络篡改，以及Go可信域/accepted绑定。

仅该可选测试需要 Node.js 22+（CI使用24.x）和npm，不改变Go运行依赖。先显式准备Go模块缓存，然后运行：

```bash
go mod download
npm --prefix interop/bankofai ci --ignore-scripts --no-audit --no-fund
npm --prefix interop/bankofai test
```

依赖版本和tarball integrity完整锁定；测试期间Node进程禁网，Go子工具禁止自动下载模块。付款人均临时随机生成、无既有资金/私钥输入，签名只经内存管道传递，不打印。上游客户端保留资产白名单/单笔限额，无自动approval；facilitator的余额/allowance是明确的模拟状态，未提供写交易能力。**这证明授权互通，不证明链上结算。**

上游1.2.0的实际网络常量为 `tron:0xcd8690dc` / `tron:0x94a9059e`。互操作测试按其真实报价注册相同网络键，不将其十六进制键偷偷改成十进制，也不修改SDK常量。项目默认仍输出规范十进制；源码较新的网络别名支持不能推断为该发布包已支持。测试单独验证其拒绝十进制报价，未宣称所有上游版本/默认HTTP流程可互通。实际对接须先统一报价/能力发现/注册的网络profile并验证具体版本。

测试覆盖地址、网络、TronWeb 编码/签名对照、授权/费用/时间边界、模拟节点和 PostgreSQL 并发恢复。固化核验覆盖历史过期交易、完整原始字节/块内归属、执行状态、头/块不一致、精确事件/重复和回转、嵌套重复 JSON、网络/超时/并发；恢复与持久化覆盖升级回滚、约束、冲突结果、迟到回调、SQL 失败和实际提交后响应丢失。facilitator 覆盖官方 SDK、HTTP鉴权/输入/容量/错误脱敏、动态临时签名、16并发单次签名/广播、过期及已消费 nonce 的历史恢复、pending/节点故障和未提交字节不广播。自动化用例的回执和区块均为合成夹具；实链接入结果单独记录在前文。Linux CI另构建/启动三个真实示例子进程，测试专用随机PostgreSQL schema与自签名TLS模拟节点；macOS系统根证书不读取SSL_CERT_FILE，因此该合成节点子进程用例明确跳过，不跳过TLS校验。测试网候选范围已完成；接入支付网关或发布版本需要另行安排，不自动操作主网。

## 构建测试网候选包

在干净、已提交的源码目录使用可信Go工具链执行（输出父目录必须为仓库外绝对路径）：

```bash
go run ./tools/package -output /absolute/path/outside-repository
```

默认构建macOS/Linux的amd64和arm64，亦可用`-targets linux/amd64`缩小矩阵；不创建Git tag或GitHub release。每个平台含`deployment`、`facilitator`、`server`、`payer`可执行文件、配置模板、两份README、数据库schema/升级SQL、项目及实际运行依赖完整许可证、源码/工具链元数据。独立源码包仅来自`git archive`，不会扫描本机忽略目录。`manifest.json`记录源码SHA和各包摘要，`SHA256SUMS`可用`shasum -a 256 -c SHA256SUMS`核对；这只能检测传输完整性，不代替可信来源/签名。

同一源码提交、Go版本和目标矩阵使用固定编译选项/归档顺序/时间/权限，可重复构建比较摘要；不同Go补丁版本不保证相同字节。工具拒绝脏工作树、已有目标目录、缺失依赖许可证或模块替换，不覆盖旧包。解包后按前述示例配置分别运行`bin/<程序>`；包含的`.env.example`不会自动加载，不得填入主网或无上限授权。候选包依然只是**未审计的测试网示例**，不是资金托管服务、生产部署承诺或稳定版本。

## 参考与发布状态

- [TRON 官方地址格式](https://developers.tron.network/docs/account)
- [x402 官方 Go SDK](https://github.com/x402-foundation/x402/tree/main/go)
- [TRON 生态参考实现](https://github.com/BofAI/x402)

使用 [Apache-2.0](LICENSE) 许可证；第三方参考与依赖见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。尚未发布稳定版本，也未通过独立安全审计。
