package exact

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	tron "github.com/eightyun/x402-tron-go"
)

var ErrEOASigner = errors.New("本地 EOA 签名器不可用或密钥无效")

// EOASigner 是可选的内存单 EOA 签名器，不加载文件/环境变量、不导出密钥。
// 密钥驻留 Go 进程，不等价于 HSM/MPC；生产部署可用外部 AuthorizationSigner 替代。
type EOASigner struct {
	mu      sync.RWMutex
	key     *secp256k1.PrivateKey
	address string
}

// NewEOASigner 复制严格 32 字节且在曲线阶内的私钥；调用方仍须管理原始输入生命周期。
func NewEOASigner(privateKey []byte) (*EOASigner, error) {
	var scalar secp256k1.ModNScalar
	defer scalar.Zero()
	if len(privateKey) != 32 || scalar.SetByteSlice(privateKey) || scalar.IsZero() {
		return nil, ErrEOASigner
	}
	key := secp256k1.NewPrivateKey(&scalar)
	hash := keccak(key.PubKey().SerializeUncompressed()[1:])
	address, _ := tron.NormalizeAddressBase58("41" + hex.EncodeToString(hash[12:]))
	return &EOASigner{key: key, address: address}, nil
}

// Address 返回可配置给 Client 的 Base58Check 付款人地址。
func (signer *EOASigner) Address() string {
	if signer == nil {
		return ""
	}
	return signer.address
}

// SignAuthorization 签署已计算的 TIP-712 摘要，不加前缀、不再次哈希。
func (signer *EOASigner) SignAuthorization(ctx context.Context, digest [32]byte) ([]byte, error) {
	return signer.signDigest(ctx, digest)
}

// SignTransaction 签署可信构建器计算的原生交易 SHA-256 摘要，不加前缀、不再次哈希。
// 必须作为 SettlementBuilder/Facilitator 的签名器使用，不能向不可信调用方暴露任意摘要签名服务。
func (signer *EOASigner) SignTransaction(ctx context.Context, digest [32]byte) ([]byte, error) {
	return signer.signDigest(ctx, digest)
}

func (signer *EOASigner) signDigest(ctx context.Context, digest [32]byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if signer == nil {
		return nil, ErrEOASigner
	}
	signer.mu.RLock()
	defer signer.mu.RUnlock()
	if signer.key == nil {
		return nil, ErrEOASigner
	}
	compact := ecdsa.SignCompact(signer.key, digest[:], false)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if compact[0] != 27 && compact[0] != 28 {
		return nil, ErrEOASigner
	}
	signature := make([]byte, 65)
	copy(signature, compact[1:])
	signature[64] = compact[0]
	return signature, nil
}

// Close 幂等停止签名并尽力清除内部标量；Go 运行时不提供内存完全擦除保证。
func (signer *EOASigner) Close() error {
	if signer == nil {
		return nil
	}
	signer.mu.Lock()
	defer signer.mu.Unlock()
	if signer.key != nil {
		signer.key.Zero()
		signer.key = nil
	}
	return nil
}

// String 避免常见格式化输出暴露内部标量。
func (*EOASigner) String() string { return "EOASigner(<redacted>)" }

// GoString 避免 %#v 输出内部标量。
func (*EOASigner) GoString() string { return "EOASigner(<redacted>)" }
