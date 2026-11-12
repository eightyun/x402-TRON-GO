# 资源服务端接入

简体中文 | [English](resource-server.md)

API 资源服务端如何生成 TRON 报价，并在可信结算成功后才交付资源。

## 接入与示例服务

`exact.NewServer(exact.ServerConfig{Network: network, Asset: asset, PayTo: payTo})` 可直接注册到官方资源服务端。固定单网络/资产/收款人，不需要数据库或钱包私钥。`ParsePrice` 接受规范最小单位正整数字符串（如 `"1000"`）、官方 `x402.AssetAmount` 或相应严格 map；**不接受 `$0.01`、浮点数或隐式汇率换算**。

机制只启用官方 `upfront`：报价自动包含 `assetTransferMethod: permit2` 和 `paymentFlow: upfront`，facilitator 结算明确成功后才运行 API handler。`pending`、`unknown`、拒绝、错误或超时都不能返回结算成功。upfront 不先调用独立的 `/verify`，所以 **`/settle` 必须自行完整验签、预检、持久化并核验固化执行与实际到账**；不能只广播就返回 `success=true`。

启动时要求可信 facilitator 的 `/supported` 包含匹配的 `x402Version: 2`、`scheme: exact`、规范网络，以及 `extra.supportedAssetTransferMethods` 数组中的 `permit2`。这个能力字段沿用已有 TRON 生态实现，不会被复制到支付报价；声明本身不是可信合约或付款证明。合约部署配置仍需在客户端和 facilitator 独立核对。

可运行的 [服务端示例](../examples/server/main.go) 使用官方 net/http 中间件。先设置以下环境变量，再运行 `go run ./examples/server`：

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

[自建 facilitator](facilitator.zh-CN.md) 示例仍需独立准备/核验合约和节点，不默认使用公共 facilitator 地址。自动化 HTTP 集成测试使用模拟链状态/回执；实链程序、打包示例的独立进程验收记录在 [Nile 实付记录](validation.zh-CN.md#nile-实付记录)，**均不等于生产验收**。
