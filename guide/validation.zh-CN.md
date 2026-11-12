# 测试与验证

简体中文 | [English](validation.md)

本页说明如何运行各项测试，以及每项测试覆盖的内容。同时记录 Nile 测试网实付结果、合约构建复现和测试网候选包的构建方法。

## 运行测试

测试需要 Go 1.26.10 或更高版本。CI 使用最新的 1.26.x 补丁版本。目前只有测试网候选版本，公开 API 仍可能变化。

以下命令与 CI 使用的包列表一致：

```bash
go mod verify
go vet . ./exact ./exact/postgres ./examples/client ./examples/payer ./internal/examplekeys ./examples/server ./examples/facilitator ./examples/deployment ./interop/bankofai/bridge ./tools/package
go test -race -count=1 -cover . ./exact ./exact/postgres ./examples/client ./examples/payer ./internal/examplekeys ./examples/server ./examples/facilitator ./examples/deployment ./interop/bankofai/bridge ./tools/package
go build . ./exact ./exact/postgres ./examples/client ./examples/payer ./internal/examplekeys ./examples/server ./examples/facilitator ./examples/deployment ./interop/bankofai/bridge
```

### 数据库测试

PostgreSQL 集成测试需要设置 `X402_TEST_DATABASE_URL`。它必须指向专用的 PostgreSQL 测试库，并具备创建临时 schema 的权限。

- 每个用例创建独立的随机 schema，结束后只清理自己的 schema。
- 未设置该变量时，集成测试会明确跳过。跳过不等于数据库验收通过。
- CI 会配置 PostgreSQL 16 并实际执行这些测试。

### Node.js

只有可选的[上游 SDK 互操作测试](#上游-tron-sdk-互操作)需要 Node.js 和 npm。其他测试只依赖 Go。

### 输入规则

输入解析是严格的。测试依赖以下规则：

- 不裁剪空白字符。
- 拒绝未知网络、通配符和带前导零的十进制数。
- 命名空间必须是 `tron`。旧版十六进制引用不区分大小写。
- 地址必须是 34 字符的 Base58Check，或以 `41` 开头的 42 字符十六进制。任意 `0x` 地址不作为 TRON 输入接受。
- 地址格式化不等于授权校验。

### 测试覆盖

| 范围 | 覆盖内容 |
| --- | --- |
| 通用 | 地址、网络、公开 TronWeb 编码与签名对照、篡改、授权、金额、费用和时间边界、模拟节点、PostgreSQL 并发与恢复 |
| 固化核验 | 历史过期交易、原始字节与块内归属、执行状态、链头与区块不一致、精确、重复与回转事件、畸形及嵌套重复 JSON、网络故障、超时与并发 |
| 恢复与持久化 | 升级与回滚、约束、冲突证据、迟到回调、SQL 失败、实际提交后响应丢失 |
| Facilitator | 官方 SDK 与 HTTP 鉴权、输入与容量限制、错误脱敏、动态临时签名、16 并发下只签名和广播一次、过期及已消费 nonce 的恢复、pending 与节点故障、从不广播未提交的字节 |

自动化用例的回执和区块均为合成夹具。实链结果单独记录在 [Nile 实付记录](#nile-实付记录)。

Linux CI 还会构建并启动三个可执行示例。它们使用随机 PostgreSQL schema 和自签名 TLS 证书的模拟节点。macOS 系统根证书不读取 `SSL_CERT_FILE`，因此该模拟节点子进程用例在 macOS 上明确跳过。这不会跳过 TLS 校验。

测试网候选范围已完成。接入支付网关和发布版本是另行安排的步骤。这里的任何流程都不会自动操作主网。

## HTTP 付款往返与原交易恢复

[三端集成测试](../exact/postgres/http_roundtrip_integration_test.go)执行一次完整的 HTTP 付款。先配置专用测试数据库，再单独运行：

```bash
go test -race -count=1 ./exact/postgres -run '^TestHTTPAutomaticPaymentRoundTripAndOriginalRecovery$'
```

测试流程：

1. 官方 HTTP 客户端收到真实的 402 报价。
2. 客户端调用 `exact.NewClient` 生成随机 nonce 和真实签名。
3. 测试 TLS 资源服务和鉴权 facilitator 服务执行预检和完整模拟，并保存日志记录。
4. 模拟节点处理广播和固化核验。
5. 成功持久化后才交付资源。

测试用例覆盖：

- 模拟被拒绝时，不占用额度、不付款、不交付。
- 超出预算的报价在签名前被拒绝。
- pending 状态下不交付。
- 使用同一授权恢复时，始终只有一笔交易。
- 成功后节点不可用，仍可从日志恢复。

失败响应也可能包含 `PAYMENT-RESPONSE`。必须检查 `success`、错误原因和原交易 ID，不能以响应头存在判定成功。

该测试能证明和不能证明的内容：

- 付款人密钥在测试内临时随机生成。不加载已有钱包，也不发起实链交易。它与人工执行的 [Nile 实付记录](#nile-实付记录)相互独立。
- 恢复复用原授权，不创建新授权。
- **防重复付款不等于防重复履约**。重放资源请求可能再次执行 handler。业务交付的幂等性仍由接入方负责。
- 它不替代示例从零部署、实链付款或进程中断恢复的验收。

## 上游 TRON SDK 互操作

[互操作测试](../interop/bankofai/interop.test.mjs)使用上游实际发布的包进行双向测试。测试调用其公开的 core、client 和 facilitator API，而不是复制其算法或只比较 JSON 字段。

- **Go 端**：Go 使用本项目的公开客户端和官方 Go core。
- **反方向**：由上游 core 和 facilitator 验证授权。
- 两个方向的签名都经过真实的密码学校验。

Nile 和 Shasta 上的覆盖范围：

- 旧版 `extra` 和 `upfront` 流程。
- 篡改签名、金额、收款人、代理、nonce、时效和网络。
- Go 的可信域与 `accepted` 绑定。

### 运行测试

该测试需要 Node.js 24.22.1 或更高版本（上游包的要求）以及 npm。CI 使用 Node 24.x。Go 运行时依赖不变。

先准备 Go 模块缓存，再安装并运行：

```bash
go mod download
npm --prefix interop/bankofai ci --ignore-scripts --no-audit --no-fund
npm --prefix interop/bankofai test
```

### 隔离措施

- 依赖版本和完整 tarball integrity 均已锁定。
- 测试期间 Node 进程禁止联网，Go 辅助工具不能下载模块。
- 付款人均为临时随机密钥。不使用已有钱包或资金。
- 签名只经内存管道传递，不写入日志。
- 上游客户端保留明确的资产白名单和单笔限额，没有自动 approval。
- facilitator 的余额与 allowance 读取均为明确的模拟状态。不提供写交易能力。

**这证明授权互通，不证明链上结算。**

### 网络标识

上游 2.1.2 使用十进制 `tron:3448148188` 和 `tron:2494104990`，与 Go 根包默认值一致。上游还会将 `tron:0xcd8690dc` 和 `tron:0x94a9059e` 规范化为十进制。

有专门的测试断言这一行为。上游一旦变化，测试会失败，需要重新复核接入。

- 测试不修改 SDK 常量。
- 不宣称与所有上游版本或默认 HTTP 流程都能互通。
- 旧版 `1.x` 只识别十六进制标识。

实际对接前，须核实对方的具体版本，以及报价、能力发现和注册所用的网络 profile。

## Nile 实付记录

所有实付均使用 Nile 测试 USDT `TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf`，以及[已复现的 Nile 合约构建](#已复现的-nile-合约构建)中所述的 Permit2 和代理合约。手续费为实际观测值，不代表未来费用。

| 编号 | 日期 | 方向 | 交易 | 结果 | 手续费（测试 TRX） |
| --- | --- | --- | --- | --- | --- |
| 1 | 2026-10-18 | Go 客户端到 Go facilitator | [135dfb80…23efe5](https://nile.tronscan.org/#/transaction/135dfb80322290839444d93864f8190ae16cb0aadb1b9d9621110f96b823efe5) | 已交付；成功终态的跨进程恢复 | 7.237 |
| 2 | 2026-10-18 | Go 客户端到 Go facilitator | [3a339895…18873a](https://nile.tronscan.org/#/transaction/3a339895a1c7f3e66370e33e8bc71c729ff287c826dcf82129fa6ffd7318873a) | 待确认中断恢复后交付 | 5.737（第 1、2 笔合计 12.974） |
| 3 | 2026-10-18 | 打包后的 Go 付款端、资源端和 facilitator | [ea6149a0…1f9249d](https://nile.tronscan.org/#/transaction/ea6149a03be449d61d31770edfba94c2d2690299d6c688a004b5a51431f9249d) | 已交付；资源端与 facilitator 重启后恢复 | 5.737（第 1 至 3 笔合计 18.711） |
| 4 | 2026-11-11 | 上游 JS 付款端到 Go 资源端和 Go facilitator | [da6c5611…71d4b82](https://nile.tronscan.org/#/transaction/da6c5611d3e7741e930f63ac775bd31e45443a2f8d6cf0f3ed35d6c0571d4b82)、[2bb10acb…7d83090](https://nile.tronscan.org/#/transaction/2bb10acb83ed2d53a023f55fa6b2cee74ce1dfa23c7f896fe164a38747d83090) | 均已交付 | 每笔结算 5.737 |
| 5 | 2026-11-11 | Go 付款端经 Go 资源端到上游 facilitator | [e6f7d422…f2379bfe](https://nile.tronscan.org/#/transaction/e6f7d422c8e20796d487f3e21eb903e5301714b83599cc2259403abdf2379bfe)、[5a63e5e7…f079e1e1d](https://nile.tronscan.org/#/transaction/5a63e5e73e20a90787bcda3c0ea724d1a03ef3477d464a59db8627df079e1e1d) | 均已固化；发现三处互操作差异 | 每笔结算 5.737，另有一次重放回滚 1.71 |

### 第 1 至 3 笔的环境

- Go 1.26.5、官方 Go SDK v2.28.0。第 1 笔使用核心版本 `39b83a63fab27715306cf6fb273a9dd7184195e5`，第 2 笔使用 `d04f500`，第 3 笔使用 `3f1beac` 的打包构建（见下文）。
- 独立的付款、执行（支付手续费）和收款地址，以及专用 PostgreSQL 数据库。
- 一次性本地接入程序调用公开 SDK API 和官方 HTTP 中间件。
- 两个服务为 loopback TLS，连接真实节点并获得真实链上回执。
- 没有修改核心实现，也没有放宽准入检查。

### 第 1 笔：首次付款与成功终态恢复

- **授权**：限额授权 0.003 测试 USDT，手续费 0。
- **付款**：由真实 402 报价生成一份 0.001 测试 USDT 的授权。
- **执行顺序**：完整模拟、保存唯一签名交易、单次广播、固化核验、成功落库，全部完成后才返回 HTTP 200 付费资源。
- **交易：**[135dfb80…23efe5](https://nile.tronscan.org/#/transaction/135dfb80322290839444d93864f8190ae16cb0aadb1b9d9621110f96b823efe5)，固化于块 `71933724`。包含唯一一笔精确 Transfer，双方余额变化一致。
- **手续费**：结算 7.237 测试 TRX。
- **恢复**：原进程正常结束后，新进程重放同一授权，从已有 `succeeded` 记录返回同一 txid。
  - 新增授权签名、交易签名和广播均为 0。
  - 原签名字节、余额及费用均不变。
- **证明内容**：成功终态的跨进程恢复。**这不是待确认期间强制中断的演练。**

### 第 2 笔：待确认中断恢复

- **中断**：结算 [3a339895…18873a](https://nile.tronscan.org/#/transaction/3a339895a1c7f3e66370e33e8bc71c729ff287c826dcf82129fa6ffd7318873a) 在日志中为 `pending`。此时固化节点的交易主体和回执接口都查不到它，资源也尚未交付。随后执行进程先被 SIGSTOP 暂停，再被 SIGKILL 终止，并确认已退出。
- **恢复**：新进程不加载私钥，复用原 header、同一数据库和 603 字节的原签名交易。
- **交付**：交易固化于块 `71934525` 后才交付。
- **计数**：新增授权签名、交易签名和广播均为 0。
- **核对**：第 1 笔的全部记录未变。付款方和收款方余额精确变化 0.001 测试 USDT。
- **手续费**：5.737 测试 TRX。第 1、2 笔合计 12.974 测试 TRX。
- **证明内容**：执行进程被杀死后，真实的待确认结算可以恢复。

### 第 3 笔：打包后的三进程接入

- **构建**：从干净提交 `3f1beac5d027598a30e1a03b2b2503880c7d06c5` 实际归档并解包出三个程序，分别独立启动。
- **环境**：复用同一专用日志库和剩余的有限授权，启用 `X402_RPC_BATCH=true`。
- **交易：**[ea6149a0…1f9249d](https://nile.tronscan.org/#/transaction/ea6149a03be449d61d31770edfba94c2d2690299d6c688a004b5a51431f9249d)，固化于块 `71935578` 后才返回 HTTP 200。
- **恢复**：资源端与 facilitator 均停止，并以新进程重启。付款端无需私钥，用原 header 恢复同一交易。603 字节签名交易、其哈希以及前两笔记录均不变。
- **数据库角色**：运行角色没有 DDL、DELETE 或 TRUNCATE 权限。结束后禁用登录，所有服务停止。
- **审计**：独立只读审计核对了唯一精确 Transfer、三笔累计 0.003 测试 USDT、手续费合计 18.711 测试 TRX，以及剩余 allowance 为 0。没有追加授权。
- **证明内容**：打包后的程序可以端到端运行，两个服务重启后仍能恢复。

### 第 4 笔：上游 JS 付款端到 Go（2026-11-11）

- **付款端**：实际发布的上游 `@bankofai/x402-tron@2.1.2` 和官方 `@x402/core@2.29.0`。只调用公开 API。签名器不具备链上读写能力。
- **服务端**：由干净提交 `1bf1019` 以 Go 1.26.11 和官方 Go SDK v2.30.0 构建的 Go 资源端与 Go facilitator。
- **数据库**：新建的专用日志库和最小权限运行角色。
- **授权**：限额授权 0.002 测试 USDT，手续费 0。
- **交易**：两笔 0.001 测试 USDT 付款，均由真实 402 报价生成。
  - [da6c5611…71d4b82](https://nile.tronscan.org/#/transaction/da6c5611d3e7741e930f63ac775bd31e45443a2f8d6cf0f3ed35d6c0571d4b82)，块 `72617132`。
  - [2bb10acb…7d83090](https://nile.tronscan.org/#/transaction/2bb10acb83ed2d53a023f55fa6b2cee74ce1dfa23c7f896fe164a38747d83090)，块 `72617154`。
- **交付**：固化核验并落库后才返回 HTTP 200。
- **审计**：独立只读审计显示付款方 −0.002、收款方 +0.002 测试 USDT，allowance 归零。
- **手续费**：每笔结算 5.737 测试 TRX。
- **证明内容**：上游 JS 付款端到 Go 资源端和 facilitator 方向的完整 HTTP 实付互通。

### 第 5 笔：Go 付款端到上游 facilitator（2026-11-11）

- **路径**：Go 付款端（`examples/payer`）经 Go 资源端，到上游 `@bankofai/x402-tron@2.1.2` facilitator。
- **facilitator 包装**：上游 facilitator 运行在本地 HTTP 包装之后，fee_limit 为 11 TRX，进程内写入次数受限。
- **交易**：两笔 0.001 测试 USDT 结算，均固化成功，双方余额精确变化。
  - [e6f7d422…f2379bfe](https://nile.tronscan.org/#/transaction/e6f7d422c8e20796d487f3e21eb903e5301714b83599cc2259403abdf2379bfe)，块 `72617474`。
  - [5a63e5e7…f079e1e1d](https://nile.tronscan.org/#/transaction/5a63e5e73e20a90787bcda3c0ea724d1a03ef3477d464a59db8627df079e1e1d)，块 `72617628`。
- **手续费**：限额授权 0.002 测试 USDT 花费 0.345 测试 TRX，每笔结算 5.737 测试 TRX。连同下文的重放回滚，本次合计 13.529 测试 TRX。
- **证明内容**：反方向付款可用。本次还发现了三处互操作差异：

1. **回执格式**。上游回执中的 `payer` 为 `0x` 签名格式，并且省略了可选的 `amount`。付款端示例原先逐字比较这些字段，误判为未确认。已在 `64f8e75` 修正：改为与已配置的付款人比对，`amount` 只在出现时才校验。
2. **重放行为**。上游 facilitator 没有持久化日志，成功后不保留记录。重新提交同一授权会再次广播，链上因 nonce 已使用而回滚（[4a491674…](https://nile.tronscan.org/#/transaction/4a491674e88d0015d53f0feae1f3d666ffd4a660d3ba851c7de6b93bc97dcc1d)，耗费 1.71 测试 TRX，未发生转账）。对接上游 facilitator 时，付款端不应对已拿到 txid 的请求自动重放。已反馈上游 [BofAI/x402#122](https://github.com/BofAI/x402/issues/122)。
3. **成功时机**。上游在交易被打包后立即返回 `success=true`，此时固化节点尚无该交易。本项目的 facilitator 只在固化并核验精确转账后才返回成功。

### 验证范围

以上实付完成了 v0.1 测试网候选范围内的 Nile 实付、中断恢复和打包三端接入。

使用条件：

- 已有的数据库 schema、经独立核验的合约和本机 loopback 服务。

不属于以下验证：

- 不是空白机器安装。
- 不是独立服务器的 TLS 运维。
- 不是生产验收。

仍未验证：

- Shasta 和主网。
- 长时间运行的稳定性。
- 独立安全审计。

其他限制：

- 公共 RPC 仍可能因链头漂移拒绝新付款。
- 私钥、原始授权和本机运维材料不公开。
- 不提供可直接照搬的生产资金配置。
- 候选包可自行构建。测试网候选版本 `v0.1.0-rc.3` 已作为 GitHub prerelease 发布。尚无稳定版本。

## 已复现的 Nile 合约构建

2026-10-18，对 Nile 上已部署合约的本地构建结果与 Nile `eth_getCode` **全字节一致，包含元数据**。

- 构建使用下表中固定的源码和编译器。
- 链接只使用编译器声明的 `immutableReferences`，填入已知的构造参数。
- 这不是仅比较 ABI、getter 或长度，也不是移除元数据后的比较，更不是直接采信节点报告的哈希。

| 项目 | Permit2 | exact 代理 |
| --- | --- | --- |
| 地址 | `TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h` | `TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F` |
| 固定源码 | [SUN Permit2 67b1f91](https://github.com/sun-protocol/sunswap-permit2/tree/67b1f91662424416e50d05d734f9fd3856039cc5)（含仓库内 solmate） | [BofAI contracts 82aa2b2](https://github.com/BofAI/x402-contracts/tree/82aa2b2b761ebe61f08b4e6ae2558846c67ded47)，OpenZeppelin 5.6.1 |
| 编译器 | solc `0.8.17+commit.8df45f5f` | solc `0.8.25+commit.b61c2a91` |
| 编译器 SHA-256 | `617828e63be485c7cc2dbcbdd5a22b582b40fafaa41016ad595637b83c90656c` | `f8c9554471ff2db3843167dffb7a503293b5dc728c8305b044ef9fd37d626ca7` |
| 参数 | optimizer 999999、viaIR、London | optimizer 999999、Cancun |
| runtime 字节数 | 9193 | 3593 |
| runtime Keccak-256 | `0xfdd5ad3c59a61e0d232cc5ec5fe684bbe3083990c0873441a6c928bb452a4705` | `0x45b702594297f3ce7ea10f4ff622608653dd3ce289445269686607ac1d5f626d` |

构建细节：

- 两者均使用 `metadata.useLiteralContent=true`，与[部署插件的设置](https://github.com/sun-protocol/sunhat/blob/2efcfb61092024c69fb86f0568edc4be5c7798b7/src/index.ts#L326)一致。
- Permit2 的 immutable 按 Nile chain ID 和 Permit2 地址计算签名域。
- 代理链接上述 Permit2 地址。
- 编译器 SHA-256 来自 [Solidity 官方编译器索引](https://github.com/ethereum/solc-bin/blob/gh-pages/wasm/list.json)。

**使用的是可复现的标准 solc 构建，不是同版本号的 TRON fork 编译器**。后者生成的代理 runtime 不一致，不能替代使用。

限制：

- 这些是经过显式核验的参考配置。**它们不会自动成为 SDK 或示例的默认值。**
- 代码复现本身不是主网配置、安全审计或付款证明。
- 链头漂移仍可能拒绝一轮预检，不能跳过该检查。
- 付款证据和未验证范围见 [Nile 实付记录](#nile-实付记录)。

## 构建测试网候选包

在干净、已提交的源码目录中，使用可信的 Go 工具链运行打包工具。输出父目录必须是仓库外的绝对路径：

```bash
go run ./tools/package -output /absolute/path/outside-repository
```

### 目标平台

- 默认构建 macOS 和 Linux 的 amd64 与 arm64。
- 可用 `-targets linux/amd64` 等参数缩小矩阵。
- 工具不创建 Git tag，也不创建 GitHub release。

### 包内容

每个平台包包含：

- `deployment`、`facilitator`、`server` 和 `payer` 可执行文件。
- 配置模板。
- 两份 README 和 guide 指南。
- 安全策略和威胁模型。
- 数据库 schema 和升级 SQL。
- 项目及所有已编译依赖的完整许可证文本。
- 源码与工具链元数据。

源码包只由 `git archive` 生成，不会包含本机被忽略的目录。

### 完整性

- `manifest.json` 记录源码版本和各归档的摘要。
- 使用 `shasum -a 256 -c SHA256SUMS` 核对。
- 摘要只能保证完整性，不代替来源认证或签名。

### 可重复构建

编译选项、归档顺序、时间戳和文件权限均已固定。同一提交、Go 版本和目标矩阵的重复构建可以比较摘要。不同 Go 补丁版本不保证生成相同字节。

工具会拒绝脏工作树、已存在的输出目录、缺失的依赖许可证和模块替换，也不会覆盖旧包。

### 运行包内程序

解包后，按[资源端](resource-server.md)和 [facilitator](facilitator.md) 指南中的配置运行 `bin/<程序>`。

- 包内的 `.env.example` 不会自动加载。
- 不得用于主网，也不得使用无上限授权。

候选包仍是**未审计的测试网示例**。它不是资金托管服务、生产部署承诺或稳定版本。
