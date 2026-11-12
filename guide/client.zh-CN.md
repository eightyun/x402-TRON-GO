# 客户端接入

简体中文 | [English](client.md)

本文介绍如何把 TRON `exact` 客户端机制注册到官方 x402 Go 客户端，包括如何提供签名器，以及如何限制客户端可以签署的付款授权。

## 运行示例

示例需要 Go 1.26.10 或更高版本。项目尚未发布稳定版本，请在克隆的仓库中运行：

```bash
go run ./examples/client
```

示例使用进程内的临时随机密钥，通过 loopback HTTP 夹具真实调用官方 x402 SDK。流程如下：

`402 → 生成 TRON 授权 → PAYMENT-SIGNATURE 重试 → 202 已验签、未结算`

该示例不会：

- 连接区块链节点；
- 使用既有钱包；
- 发起交易；
- 返回成功的结算回执。

它不是完整的链上闭环。示例中的地址只是公开的协议夹具，不能作为可信部署配置。

## 注册机制

授权字段、nonce 和 TIP-712 摘要都由机制自动生成，无需自行拼装。按如下方式注册到官方客户端：

```go
import (
    "net/http"
    "time"

    "github.com/eightyun/x402-tron-go/exact"
    x402 "github.com/x402-foundation/x402/go/v2"
    x402http "github.com/x402-foundation/x402/go/v2/http"
)

// contracts 必须来自独立核验的可信部署配置。
// 金额均为代币最小单位。
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

`ClientConfig` 字段：

| 字段 | 含义 |
| --- | --- |
| `Authorization` | 可信合约配置（网络、Permit2 地址和代理合约地址）。 |
| `PayerAddress` | 付款人的 TRON 地址，例如 `signer.Address()`。 |
| `Asset` | 客户端愿意支付的 TRC-20 代币。 |
| `PayTo` | 信任的收款人。 |
| `MaxAmount` | 单次付款上限，以代币最小单位表示，不能为零。 |
| `Lifetime` | 每个授权的有效期。默认 `60` 秒，须为整秒，范围 `6` 秒至 `10` 分钟。 |
| `Signer` | 签署 TIP-712 摘要的函数，见[签名器](#签名器)。 |

任一字段无效时，`NewClient` 返回 `ErrClientConfig`。

## 签名器

### 内置 EOA 签名器

`exact.NewEOASigner(keyBytes)` 是可选的内存签名器，只管理单个 EOA。

- 只接受严格有效、非零的 32 字节私钥，并将其复制到内存中。
- 原始密钥字节仍由调用方管理。使用完毕后调用 `Close()`。
- 不从文件或环境变量加载密钥，也无法导出密钥。
- 常见格式化（`String`、`GoString`）和 JSON 输出都不会暴露内部标量。
- `Close()` 只是尽力清除内存，不能替代安全硬件。

### 外部签名器

生产环境可以通过 `ClientConfig.Signer` 接入自己的签名器。签名器收到的是**已计算好的 TIP-712 摘要**，必须原样签署：

- 不要再次哈希；
- 不要添加 `personal_sign` 前缀；
- 不要与结算所用的交易 SHA-256 摘要签名混用。

外部签名器必须支持并发调用，并遵守有界 context。客户端不会透传签名器的原始错误信息，统一脱敏为 `ErrClientSign`。

## 客户端检查项

签名前，客户端会按配置检查报价：

- 网络、资产和收款人必须与配置一致；
- 金额不得超过单次上限 `MaxAmount`；
- 金额不得超过官方 SDK 传入的上限（来自 `SpendControls` 的 `MaxAmountPerPayment`）；
- 拒绝未知的 `extra` 字段和非空扩展。

违反付款策略的报价返回 `ErrClientPolicy`。

授权的实际有效期取 `Lifetime` 与报价超时中的较小值；如果不足 6 秒，创建授权会返回 `ErrInvalidValidity`。签名完成后，客户端会再次检查签名和剩余有效期。

客户端不查询余额或授权额度。付款人必须事先向 Permit2 合约授予代币额度。

## 限制与不保证事项

> [!WARNING]
> **单次上限不是累计预算。** 每次创建付款载荷都会产生一个新的、可被扣款的授权。付款结果未知时，不要自动重新付款。

- 不要从不可信的 402 报价中获取可信策略（资产、收款人或合约）。
- 授权不绑定 URL 或请求内容。
- 应用必须自行限制可信站点和预算。
- 拒绝不安全的重定向。上面的示例通过 `http.ErrUseLastResponse` 在第一次重定向时停止。
- 避免恢复 hooks 额外签发授权。
- HTTP 2xx 响应和本地验签结果都不能证明款项已经上链到账。
