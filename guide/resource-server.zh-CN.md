# 资源服务端接入

简体中文 | [English](resource-server.md)

本文介绍 API 资源服务端如何生成 TRON 支付报价，以及如何确保只有在可信 facilitator 确认结算后才运行 handler。文中也说明了示例服务端的配置。

## 注册机制

将 `exact.NewServer` 注册到官方 x402 资源服务端：

```go
exact.NewServer(exact.ServerConfig{Network: network, Asset: asset, PayTo: payTo})
```

- 每个机制只服务一个可信网络、一种资产和一个收款地址。
- 资源服务端不需要数据库，也不需要钱包私钥。

## 价格

`ParsePrice` 接受以下格式：

- 以最小单位表示的规范正整数字符串，例如 `"1000"`。
- 官方 `x402.AssetAmount` 值。
- 与 `x402.AssetAmount` 严格等价的 map。

**不接受** `$0.01` 这类美元价格、浮点数价格或隐式汇率换算。

## 支付流程

机制只启用官方 `upfront` 流程。每个报价都会包含 `assetTransferMethod: permit2` 和 `paymentFlow: upfront`。

只有可信 facilitator 报告结算成功后，handler 才会运行。`pending`、`unknown`、被拒绝、出错或超时的结果，都不能作为成功返回。

`upfront` 流程不会先调用 `/verify`。因此 facilitator 的 `/settle` 端点必须自行完成以下全部工作：

- 完整验证授权并执行预检。
- 持久化记录提交。
- 确认交易已固化，并且代币确实到账。

> [!WARNING]
> 广播被接受不等于成功。facilitator 不能只完成广播就返回成功。

## Facilitator 能力发现

启动时，facilitator 的 `/supported` 响应必须声明：

- `x402Version: 2`
- `scheme: exact`
- 与配置一致的规范网络标识。
- `extra.supportedAssetTransferMethods` 数组中包含 `permit2`。

`extra.supportedAssetTransferMethods` 是 TRON 生态已有的能力字段，不会被复制到支付报价中。该声明本身不能证明合约可信，也不能证明已经结算。请独立核对客户端与 facilitator 使用的是同一套合约部署。

## 示例服务端

可运行的 [服务端示例](../examples/server/main.go) 使用官方 `net/http` 中间件。设置下列环境变量后，运行 `go run ./examples/server`。

| 变量 | 是否必填 | 含义 |
| --- | --- | --- |
| `X402_NETWORK` | 否 | 网络标识，默认为 Nile：`tron:3448148188`。 |
| `X402_ASSET` | 是 | 可信代币地址。 |
| `X402_PAY_TO` | 是 | 商户收款地址。 |
| `X402_PRICE_ATOMIC` | 是 | 以最小单位表示的正整数价格。 |
| `X402_FACILITATOR_URL` | 是 | 可信 facilitator 的 HTTPS 基础 URL，不能包含 userinfo、query 或 fragment。 |
| `X402_FACILITATOR_TOKEN` | 使用本项目 facilitator 时必填 | 通过官方 `AuthProvider` 以 Bearer token 形式发送给 facilitator 的全部三个端点。第三方 facilitator 不需要认证时可不设置。 |
| `X402_ALLOW_INSECURE_LOCAL_FACILITATOR_HTTP` | 否 | 默认关闭。只有显式设为 `true` 时才允许明文 HTTP，且仅限字面 loopback IP 地址。`localhost` 和公网地址仍会被拒绝。 |
| `X402_LISTEN_ADDR` | 否 | 监听地址，默认为 `127.0.0.1:8080`。 |

### 行为

- 开始监听前，服务端会先初始化机制并校验报价。任何配置或能力校验失败都会终止启动。
- 未附带付款时，`GET /resource` 返回 `402`。
- `HEAD` 请求会被拒绝。Go 的 `GET` 路由同样匹配 `HEAD`，否则可借此绕过付款闸门。
- 启动、结算和 HTTP 超时均有上限。
- 不跟随 facilitator 返回的重定向。
- 请求头大小有上限。
- 支持优雅关闭。

对外部署时，应在服务端前加 TLS 入口并配置适当的鉴权。facilitator 凭证应通过官方 `AuthProvider` 传递，不要把 token 拼进 URL。

### 超时

| 预算 | 取值 | 作用 |
| --- | --- | --- |
| 远程 facilitator 请求 | `200` 秒 | 覆盖机制最多 `180` 秒的预算。 |
| 支付中间件 | `410` 秒 | 允许官方对 pending 结果携带原 txid、以相同 payload 重试一次。 |
| HTTP 写超时 | `420` 秒 | 同样覆盖这次重试，并比中间件预算留有余量。 |
| 启动能力发现 | `30` 秒 | 限制启动时 `/supported` 检查的耗时。 |

反向代理和调用方也必须使用相匹配的预算。

> [!WARNING]
> 超时不代表付款失败，也不意味着可以再次付款。超时后不要创建新的授权。

## 限制

- [自建 facilitator](facilitator.zh-CN.md) 需要你自行准备并核验合约和节点，不会默认使用任何公共 facilitator 地址。
- 自动化 HTTP 集成测试使用模拟的链状态和回执。
- 实链程序与打包示例的独立运行记录见 [Nile 实付记录](validation.zh-CN.md#nile-实付记录)。**两者都不等于生产验收。**
