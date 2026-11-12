# 客户端接入

简体中文 | [English](client.md)

如何把 TRON `exact` 客户端机制注册到官方 x402 Go 客户端、提供签名器并限制客户端可签署的授权。

## 快速体验与注册

要求 Go 1.26.10 或更高；尚未发布稳定版本。克隆仓库后运行：

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
