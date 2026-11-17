# 安全策略

## 支持范围

安全修复只发布在最新的 `v0.x` 版本及 `main` 分支。本项目**未经独立安全审计**，也尚未在主网完成实付；动用真实资金前请自行核验并从小额开始。

## 报告漏洞

请**不要**通过公开 issue、PR 或讨论区报告安全问题。

请使用 GitHub 私密漏洞报告：仓库页面 → **Security** → **Report a vulnerability**（<https://github.com/eightyun/x402-tron-go/security/advisories/new>）。

报告中请尽量包含：

- 受影响的包、函数与版本（tag 或 commit）；
- 复现步骤或最小 PoC；
- 影响评估（例如重复结算、未付款交付、资金或密钥泄露）。

请勿在报告中附带真实私钥、助记词或主网资金信息；如需演示，请使用测试网和临时随机密钥。

## 处理流程

收到报告后会先确认问题并评估影响，修复后发布新版本，并在发布说明和 GitHub Security Advisory 中说明受影响版本与升级方式。

威胁假设、信任边界与已知残余风险见 [THREAT_MODEL.md](THREAT_MODEL.md)。
