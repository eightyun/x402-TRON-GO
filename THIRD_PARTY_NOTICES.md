# Third-party notices

本项目独立实现 TRON 支付机制，不隶属于下述组织。依赖的许可证和版权仍由其原作者保留。

- x402 Foundation Go SDK v2.28.0：仅引用公开 types 包，不复制协议核心。Apache-2.0，来源 https://github.com/x402-foundation/x402/tree/go/v2.28.0/go 。
- secp256k1/v4 v4.4.1：ISC，来源 https://github.com/decred/dcrd/tree/master/dcrec/secp256k1 。
- pgx/v5 v5.7.6 及 pgpassfile、pgservicefile、puddle/v2：可选 PostgreSQL 适配器使用，MIT，来源 https://github.com/jackc/pgx 、https://github.com/jackc/pgpassfile 、https://github.com/jackc/pgservicefile 和 https://github.com/jackc/puddle 。
- golang.org/x/crypto v0.46.0、x/sys v0.40.0、x/sync v0.19.0 及 x/text v0.32.0：BSD-3-Clause，来源 https://go.googlesource.com/crypto/ 、https://go.googlesource.com/sys/ 、https://go.googlesource.com/sync/ 和 https://go.googlesource.com/text/ 。
- TronWeb 6.3.0：仅作为离线授权/结算交易向量生成、ABI/Protobuf 编码和验签工具，不是 Go 运行依赖；MIT，来源 https://github.com/tronprotocol/tronweb 。公开向量使用合成区块，不包含私钥。
- BANK OF AI TRON mechanism，参考版本 e50e9f09149203a97e35110ee1cd64073487f30d：用于核对 wire 字段、签名域和二字段 witness。来源 https://github.com/BofAI/x402/tree/e50e9f09149203a97e35110ee1cd64073487f30d/typescript/packages/mechanisms/tron 。保留仓库级 MIT 声明如下；不表示对其所有软件或合约安全作出保证。

MIT License

Copyright (c) 2025 bankofai

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
