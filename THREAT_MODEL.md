# 威胁模型

本文档面向安全审计与集成方，汇总 x402-tron-go 的资产、信任边界、主要威胁、现有缓解措施及已知残余风险。实现细节以 [README](README.md) 各节和源码为准；本文不构成安全保证。**当前为未经独立审计的测试网候选版本，不可承载真实资金。**

## 1. 范围

| 组件 | 路径 | 运行位置 |
| --- | --- | --- |
| 地址/网络工具 | 根包 | 任意 |
| 付款客户端机制 | `exact.NewClient` | 付款方/Agent 进程 |
| 资源服务端机制 | `exact.NewServer` | API 服务进程 |
| 离线验签、只读预检、交易构建、广播、固化核验 | `exact` | facilitator 进程 |
| 持久化 facilitator、HTTP 入口、进程内入口 | `exact/postgres` | facilitator 进程 + PostgreSQL |

不在范围内：`examples/`、`tools/`、`interop/` 仅为演示/测试；链上 Permit2 与代理合约本身；TRON 节点实现；上游 x402 SDK 内部实现（仅审计本项目对其的使用方式）。

## 2. 资产

1. **付款方资金**：通过 Permit2 授权可被扣划的 TRC20 余额。
2. **付款授权（签名）**：持有者在有效期内可提交一次扣款，属于可直接兑现的凭证。
3. **执行账户（executor）私钥与 TRX/能量**：支付链上交易费用。
4. **结算日志**：PostgreSQL 中的占用、原签名交易与确认结果，是防重放与恢复的唯一依据。
5. **资源服务端交付物**：付款后才应交付的 API 内容。
6. **Facilitator 服务令牌**：HTTP 入口的 Bearer 令牌。

## 3. 参与方与信任假设

| 参与方 | 信任 | 说明 |
| --- | --- | --- |
| 部署方配置（网络、Permit2/代理地址与代码哈希、资产、收款人、限额、节点、创世块 ID） | 可信 | 必须独立核验；SDK 不从请求或节点学习这些值 |
| 签名器（付款方 / executor） | 可信 | 由集成方提供并保护；需支持并发与有界 context |
| 配置的 TRON 节点（JSON-RPC / FullNode / Solidity） | 半可信 | 假定诚实但可能故障、延迟或返回畸形数据；不防御恶意节点伪造状态 |
| PostgreSQL | 可信 | 独占、最小权限角色；所有 facilitator 实例共享同一日志 |
| 资源服务端 → facilitator 请求 | 半可信 | HTTP 入口需令牌；内容仍逐字段严格校验 |
| 付款请求方 / 402 报价 / 任意网络输入 | **不可信** | 包括付款载荷、报价、HTTP 头和请求体 |
| 上游 x402 SDK、TronWeb 等依赖 | 可信（固定版本） | 版本与校验和锁定；CI 运行 govulncheck |

## 4. 信任边界与数据流

```
付款方/Agent ──(402 报价, 不可信)──▶ exact.Client ──签名器──▶ PAYMENT-SIGNATURE
      │                                                          │
      ▼                                                          ▼
资源服务端 exact.Server ──(verify/settle, 令牌)──▶ facilitator HTTP / 进程内入口
                                                             │
                     ┌───────────────┬──────────────────────┼──────────────────┐
                     ▼               ▼                      ▼                  ▼
               只读 JSON-RPC 预检  executor 签名器      PostgreSQL 结算日志   FullNode 广播 / Solidity 核验
```

关键边界：(a) 报价与付款载荷进入客户端/facilitator；(b) facilitator HTTP 入口；(c) 节点响应进入 SDK；(d) 结算日志的状态转换；(e) 交易字节交给签名器与节点。

## 5. 威胁与缓解

| # | 威胁 | 缓解（代码位置） | 残余风险 |
| --- | --- | --- | --- |
| T1 | 恶意 402 报价诱导客户端签署超额、错误资产/收款人或长效授权 | 客户端固定网络/资产/收款人/单次 `MaxAmount`，叠加官方 SDK 金额上限；有效期 6 秒~10 分钟取较小值；签名后复验（`exact/client.go`） | 单次上限不是累计预算；授权不绑定 URL/请求内容 |
| T2 | 伪造或篡改授权（金额、收款人、spender、nonce、期限、网络） | TIP-712/Permit2 见证完整重算并验签，可信签名域不接受请求覆盖（`exact/authorization.go`）；双向互操作篡改测试 | 依赖链上合约与代码哈希的独立核验 |
| T3 | 同一授权重复结算 / 并发双花 | 单行事务锁内占用与 CAS 状态转换，只有 `BeginBroadcast` 成功才可广播，已 unknown/pending/succeeded 不再发送（`exact/postgres/store.go`、`facilitator.go`） | 签名后、提交前崩溃可能重复调用签名器，不保证 HSM 恰好一次 |
| T4 | 超时/断线后重新生成授权或重签，导致重复扣款 | 恢复只认原授权与原签名交易；pending/unknown 携原 txid 返回，禁止释放占用或新建付款（`recovery.go`、`facilitator.go`） | 集成方若清空日志或换新目录重试会破坏该保证；对接无持久化日志的上游 facilitator 时，重放已成功的授权会再次广播并耗费手续费（链上回滚，不重复扣款） |
| T5 | 节点受理或回执被误判为到账，提前交付资源 | 只有 Solidity 固化区块内、执行成功且精确匹配 Transfer 事件的原交易才确认成功，随后才返回 `success=true`（`exact/receipt.go`、`ConfirmSettlement`） | 不验证 Merkle 证明，不防御恶意节点伪造固化状态 |
| T6 | 畸形/超大/歧义的节点响应导致 panic、误判或资源耗尽 | 响应上限 1 MiB、禁止重定向、拒绝重复键与尾随内容、原生整数严格解析、创世块前后核对、错误脱敏（`exact/rpc.go`、`execution.go`）；`FuzzExecutionLookupBody` | 慢节点或链头漂移可能导致保守拒绝 |
| T7 | 恶意 HTTP 请求绕过输入校验或耗尽资源 | Bearer 令牌（常量时间比较），64 KiB 上限，拒绝重复/大小写歧义键、未知字段（逐层，含 `accepted`）、尾随内容和非 v2；并发上限 1~64，满载 503 且不签名/广播（`exact/postgres/http.go`）；`FuzzStrictPaymentDecoding` | 并发上限不是限流或费用预算；TLS、入口鉴权与限流由部署方负责 |
| T8 | 进程内入口被宿主内不可信代码调用 | 与 HTTP 入口相同的输入校验与并发上限（`exact/postgres/local.go`） | 无额外鉴权边界，必须由宿主应用控制访问 |
| T9 | 构造或广播非预期交易（附带 TRX/TRC10、额外合约、替换费用） | 受限交易构建，`ValidateSigned` 只接受对应准备对象的规范编码并核对全部 raw 字节、ID 与签名（`exact/transaction.go`）；`FuzzValidateSettlementTransaction` | 账户权限、能量/带宽充足性由运营方保证 |
| T10 | 预检通过但结算失败，白白消耗执行账户费用 | 新付款必须通过完整 settle 只读模拟后才占用、签名（`exact/preflight.go`） | 模拟不预留资金，链状态可在模拟后变化 |
| T11 | 跨网络重放（测试网授权用于主网等） | 网络标识进入签名域，规范化为十进制 CAIP-2，未知网络拒绝；节点链 ID/创世块核对（根包、`preflight.go`） | — |
| T12 | 敏感信息泄露（私钥、签名、令牌、节点消息） | 错误固定分类、不回显原始节点消息/URL/API key；HTTP 响应不含签名与令牌（测试断言） | 集成方日志需自行避免记录载荷 |
| T13 | 依赖漏洞 | 版本与校验和锁定；CI 每次提交及每周运行 govulncheck 并使用最新 Go 补丁版；Dependabot 安全更新 | 新披露漏洞存在发现窗口 |
| T14 | 结算日志被篡改或降级回滚 | 运行角色无 DDL/DELETE/TRUNCATE 权限；状态单向推进，确认记录不覆盖；schema 需显式应用（`exact/postgres/schema.sql`） | 数据库管理员级攻击不在防护范围 |

## 6. 明确不支持 / 不防护

- 恶意或被攻陷的配置节点伪造链状态；
- fee-on-transfer、rebase、自转账或自定义事件语义的代币；多签/合约钱包；
- 主网资金、累计预算、业务履约幂等与失败补偿；
- 签名器、数据库与宿主进程本身被攻陷。

## 7. 现有验证

- 单元/集成测试：race 模式，PostgreSQL 真实并发恢复、合成链与模拟节点。
- Fuzz：地址、网络、授权验证、交易校验、恢复、节点 Lookup 响应、facilitator 请求解码共 8 个目标，种子随常规测试执行；2026-11-11 每个目标各模糊测试 3 分钟（合计约 1.8 亿次执行），未发现失败。
- 互操作：与已发布上游 TRON JS SDK 离线双向授权验证及篡改拒绝。
- Nile 测试网：真实付款、成功终态恢复、待确认期间进程强制中断后的原交易恢复，以及双向跨 SDK 实付（上游 JS 付款端 → Go facilitator；Go 付款端 → 上游 facilitator）。
- 依赖扫描：govulncheck（CI 门禁）与 Dependabot 安全更新。

## 8. 建议审计重点

1. 结算日志状态机与并发（T3、T4）：`exact/postgres/store.go`、`facilitator.go`。
2. 固化与精确到账核验（T5）：`exact/receipt.go`。
3. 授权验签与签名域（T2、T11）：`exact/authorization.go`。
4. 受限交易编码与校验（T9）：`exact/transaction.go`。
5. 外部输入解析（T6、T7）：`exact/rpc.go`、`execution.go`、`exact/postgres/http.go`、`local.go`。
