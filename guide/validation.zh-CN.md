# 测试与验证

简体中文 | [English](validation.md)

如何运行测试、上游 SDK 互操作测试，以及 Nile 实付记录、合约复现和测试网打包。

## 运行测试

```bash
go mod verify
go vet . ./exact ./exact/postgres ./examples/client ./examples/server ./examples/facilitator
go test -race -count=1 -cover . ./exact ./exact/postgres ./examples/client ./examples/server ./examples/facilitator
go build . ./exact ./exact/postgres ./examples/client ./examples/server ./examples/facilitator
```

PostgreSQL 集成测试要求 `X402_TEST_DATABASE_URL` 指向专用测试库，并授予创建临时 schema 的权限。每个用例使用独立随机 schema，结束后仅清理自己的 schema；未设置时明确跳过数据库测试，不能当作集成验证通过。CI 配置临时 PostgreSQL 16 并实际执行这些测试。

### 完整 HTTP 付款与原交易恢复

设置专用测试数据库后，可单独运行[三端集成测试](../exact/postgres/http_roundtrip_integration_test.go)：

```bash
go test -race -count=1 ./exact/postgres -run '^TestHTTPAutomaticPaymentRoundTripAndOriginalRecovery$'
```

官方 HTTP 客户端从实际 402 报价调用 `exact.NewClient` 生成随机 nonce/真实签名，再经测试 TLS 资源服务和鉴权 facilitator 完成预检、完整模拟、日志保存、合成节点广播/固化核验、成功落库后交付。覆盖模拟拒绝零占用/付款/交付、超额报价签名前拒绝，以及 pending 不交付、原授权恢复保持唯一交易、成功后节点不可用仍从日志恢复。失败也可能包含 `PAYMENT-RESPONSE`，必须检查 `success`、错误原因及原 txid，不能以响应头存在判成功。

该自动化测试的付款人密钥仅在测试内随机生成；不会加载已有钱包或发起实链交易，与前述人工执行的 Nile 验证分开。恢复原请求不创建新授权；**防重复付款不等于防重复履约**，资源请求重放可能再次执行 handler，业务交付幂等仍由接入方负责。这不替代示例从零部署、实链付款或进程中断恢复验收。

### 上游 TRON SDK 双向互操作

[互操作测试](../interop/bankofai/interop.test.mjs) 使用上游实际发布包的公开 core/client/facilitator，而非复制其算法或只比较JSON字段。Go通过本项目公开客户端/官方core生成授权，反向由上游core/facilitator验证；签名均为真实密码学校验。Nile/Shasta覆盖旧版extra和upfront、签名/金额/收款人/代理/nonce/时效/网络篡改，以及Go可信域/accepted绑定。

仅该可选测试需要 Node.js 24.22.1+（上游包要求，CI使用24.x）和npm，不改变Go运行依赖。先显式准备Go模块缓存，然后运行：

```bash
go mod download
npm --prefix interop/bankofai ci --ignore-scripts --no-audit --no-fund
npm --prefix interop/bankofai test
```

依赖版本和tarball integrity完整锁定；测试期间Node进程禁网，Go子工具禁止自动下载模块。付款人均临时随机生成、无既有资金/私钥输入，签名只经内存管道传递，不打印。上游客户端保留资产白名单/单笔限额，无自动approval；facilitator的余额/allowance是明确的模拟状态，未提供写交易能力。**这证明授权互通，不证明链上结算。**

上游2.1.2的网络常量为十进制 `tron:3448148188` / `tron:2494104990`，与 Go 根包默认一致，并将 `tron:0xcd8690dc` / `tron:0x94a9059e` 规范化为十进制；测试单独断言这一点，上游变化时会失败并要求重新复核。测试不修改SDK常量，也未宣称所有上游版本/默认HTTP流程可互通；旧版 `1.x` 只认十六进制。实际对接仍须验证对方具体版本及报价/能力发现/注册的网络profile。

测试覆盖地址、网络、TronWeb 编码/签名对照、授权/费用/时间边界、模拟节点和 PostgreSQL 并发恢复。固化核验覆盖历史过期交易、完整原始字节/块内归属、执行状态、头/块不一致、精确事件/重复和回转、嵌套重复 JSON、网络/超时/并发；恢复与持久化覆盖升级回滚、约束、冲突结果、迟到回调、SQL 失败和实际提交后响应丢失。facilitator 覆盖官方 SDK、HTTP鉴权/输入/容量/错误脱敏、动态临时签名、16并发单次签名/广播、过期及已消费 nonce 的历史恢复、pending/节点故障和未提交字节不广播。自动化用例的回执和区块均为合成夹具；实链接入结果单独记录在下文 [Nile 实付记录](#nile-实付记录)。Linux CI另构建/启动三个真实示例子进程，测试专用随机PostgreSQL schema与自签名TLS模拟节点；macOS系统根证书不读取SSL_CERT_FILE，因此该合成节点子进程用例明确跳过，不跳过TLS校验。测试网候选范围已完成；接入支付网关或发布版本需要另行安排，不自动操作主网。

## Nile 实付记录

核心版本 `39b83a63fab27715306cf6fb273a9dd7184195e5`、Go 1.26.5、官方 Go SDK v2.28.0，使用下文已复现的 Permit2/代理、Nile 测试 USDT `TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf`、独立付款/费用/收款地址与专用 PostgreSQL。一次性本地接入程序调用公开 SDK 和官方 HTTP 中间件，两个服务为 loopback TLS，节点与链上回执真实；没有修改核心实现或放宽预检。

- 限额授权 0.003 测试 USDT 后，由真实 402 报价生成一份 0.001 测试 USDT 授权，完整模拟、保存唯一签名交易、单次广播、固化核验并落库后才返回 HTTP 200 付费资源。
- 结算 [135dfb80…23efe5](https://nile.tronscan.org/#/transaction/135dfb80322290839444d93864f8190ae16cb0aadb1b9d9621110f96b823efe5)，固化块 `71933724`；核对唯一精确 Transfer 与双方余额变化。授权手续费 0、结算手续费 7.237 测试 TRX，仅代表本次实际费用。
- 原进程正常结束后，新进程重放同一授权，从已有 `succeeded` 记录返回同一 txid；新增授权签名、交易签名和广播均为 0，原签名字节、余额及费用不变。**这是成功终态的跨进程恢复，不是待确认期间强制中断的实链演练**。

第二次实付验证了真实待确认中断：结算 [3a339895…18873a](https://nile.tronscan.org/#/transaction/3a339895a1c7f3e66370e33e8bc71c729ff287c826dcf82129fa6ffd7318873a) 在日志为 `pending`、固化主体/回执均不存在且未交付时，执行进程被 SIGSTOP 后 SIGKILL 并确认退出。新进程不加载私钥，复用原header、同一数据库和603字节原签名交易，固化于块 `71934525` 后才交付；新增授权签名、交易签名、广播均为0。第一笔全部记录未变，双方余额精确变化0.001测试USDT，第二笔费5.737测试TRX，两笔累计12.974测试TRX。

第三次使用干净提交`3f1beac5d027598a30e1a03b2b2503880c7d06c5`实际归档/解包后的三个程序独立启动，复用同一专用日志库和剩余有限授权，启用`X402_RPC_BATCH=true`。结算[ea6149a0…1f9249d](https://nile.tronscan.org/#/transaction/ea6149a03be449d61d31770edfba94c2d2690299d6c688a004b5a51431f9249d)固化于块`71935578`后返回HTTP200；服务端与facilitator均停止并以新进程重启，payer无需私钥，用原header恢复同一交易，原603字节摘要及前两笔记录完全不变。运行数据库角色没有DDL/DELETE/TRUNCATE权限，结束后禁用登录，服务全部停止。独立只读审计核对唯一精确Transfer、三笔累计0.003测试USDT、手续费18.711测试TRX、allowance为0；没有追加授权。

2026-11-11 完成**跨 SDK 实付**：付款端为实际发布的上游 `@bankofai/x402-tron@2.1.2` + 官方 `@x402/core@2.29.0`（仅调用公开 API，签名器不提供链上读写），访问由干净提交 `1bf1019` 以 Go 1.26.11、官方 Go SDK v2.30.0 构建的 Go 资源端与 Go facilitator，使用新建专用日志库与最小权限运行角色。先限额授权 0.002 测试 USDT（手续费 0），再由真实 402 报价生成并完成两笔 0.001 测试 USDT 付款：[da6c5611…71d4b82](https://nile.tronscan.org/#/transaction/da6c5611d3e7741e930f63ac775bd31e45443a2f8d6cf0f3ed35d6c0571d4b82)（块 `72617132`）与 [2bb10acb…7d83090](https://nile.tronscan.org/#/transaction/2bb10acb83ed2d53a023f55fa6b2cee74ce1dfa23c7f896fe164a38747d83090)（块 `72617154`），均在固化核验并落库后才返回 HTTP 200。独立只读审计：付款方 −0.002、收款方 +0.002 测试 USDT，allowance 归零，两笔结算费各 5.737 测试 TRX。这证明上游 JS 付款端 → Go 资源端/facilitator 方向的完整 HTTP 实付互通。

同日完成**反方向实付**：Go 付款端（`examples/payer`）→ Go 资源端 → 上游 `@bankofai/x402-tron@2.1.2` facilitator（经本地 HTTP 包装，fee_limit 11 TRX、进程内写入次数受限）。两笔 0.001 测试 USDT 结算 [e6f7d422…f2379bfe](https://nile.tronscan.org/#/transaction/e6f7d422c8e20796d487f3e21eb903e5301714b83599cc2259403abdf2379bfe)（块 `72617474`）与 [5a63e5e7…f079e1e1d](https://nile.tronscan.org/#/transaction/5a63e5e73e20a90787bcda3c0ea724d1a03ef3477d464a59db8627df079e1e1d)（块 `72617628`）均固化成功，双方余额精确变化。实测发现三处互操作差异：

- 上游回执的 `payer` 为 `0x` 签名格式，且省略可选 `amount`；原付款端示例逐字比较导致误判未确认，已在 `64f8e75` 修正为与已配置付款人比对、`amount` 出现时才校验。
- 上游 facilitator 无持久化日志，成功后不保留记录：重发同一授权会再次广播，链上因 nonce 已用而回滚（[4a491674…](https://nile.tronscan.org/#/transaction/4a491674e88d0015d53f0feae1f3d666ffd4a660d3ba851c7de6b93bc97dcc1d)，耗费 1.71 测试 TRX，未发生转账）。对接上游 facilitator 时，付款端不应对已拿到 txid 的请求自动重放；已反馈上游 [BofAI/x402#122](https://github.com/BofAI/x402/issues/122)。
- 上游在交易打包后即返回 `success=true`，此时固化节点尚无该交易；本项目 facilitator 只在固化并核验精确转账后返回成功。

这完成了v0.1测试网候选范围的Nile实付、中断恢复和实际打包三端接入。它使用已有数据库/已核验合约和本机loopback服务，不是空白机器安装、独立服务器TLS运维或生产验收；公共RPC仍可能因链头变化拒绝新付款。Shasta/主网、长时稳定性及独立安全审计仍未验证。私钥、原始授权和本机运维材料不公开，不提供可照搬的生产资金配置。候选包可自行构建；已发布测试网候选版本 `v0.1.0-rc.3`（GitHub prerelease），尚无稳定版本。

## 已复现的 Nile 合约构建

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

## 构建测试网候选包

在干净、已提交的源码目录使用可信Go工具链执行（输出父目录必须为仓库外绝对路径）：

```bash
go run ./tools/package -output /absolute/path/outside-repository
```

默认构建macOS/Linux的amd64和arm64，亦可用`-targets linux/amd64`缩小矩阵；不创建Git tag或GitHub release。每个平台含`deployment`、`facilitator`、`server`、`payer`可执行文件、配置模板、两份README、guide 指南、安全策略与威胁模型、数据库schema/升级SQL、项目及实际运行依赖完整许可证、源码/工具链元数据。独立源码包仅来自`git archive`，不会扫描本机忽略目录。`manifest.json`记录源码SHA和各包摘要，`SHA256SUMS`可用`shasum -a 256 -c SHA256SUMS`核对；这只能检测传输完整性，不代替可信来源/签名。

同一源码提交、Go版本和目标矩阵使用固定编译选项/归档顺序/时间/权限，可重复构建比较摘要；不同Go补丁版本不保证相同字节。工具拒绝脏工作树、已有目标目录、缺失依赖许可证或模块替换，不覆盖旧包。解包后按前述示例配置分别运行`bin/<程序>`；包含的`.env.example`不会自动加载，不得填入主网或无上限授权。候选包依然只是**未审计的测试网示例**，不是资金托管服务、生产部署承诺或稳定版本。
