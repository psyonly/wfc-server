package gts

// GameStats (gamestats2) 请求体的解码。
//
// 第四世代宝可梦的 GTS 走 gamestats2.gs.nintendowifi.net 上的
//
//	GET /pokemondpds/worldexchange/<handler>.asp?pid=<pid>[&hash=<h>&data=<d>]
//
// 第一次请求不带 hash，服务端回一个 32 字符 token 当作"挑战值"；紧接着第二次请求带上
//
//	hash = sha1(salt + token)                      小写十六进制
//	data = urlsafe-base64(4 字节混淆校验和 + 4 字节 pid + 负载)
//
// pid 与负载用一个线性同余流密码加密。
//
// 参数与算法取自 mm201/pkmn-classic-framework 的
// GamestatsBase/GamestatsHandler.cs 与 gts/pokemondpds.ashx.cs，
// 并用真实主机抓包校验过（见 gts_crypto_test.go）。

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// GtsCrypto 描述某个 gamestats 游戏使用的加密参数。
type GtsCrypto struct {
	// Salt 是 sha1(salt + token) 里的盐值（20 字符），
	// 同时也是响应 v2 尾部校验和的盐。
	Salt string

	// 线性同余生成器: next = (prev*mul + add) % mod
	RngMul uint32
	RngAdd uint32
	RngMod uint32

	// 校验和混淆掩码
	ChecksumMask uint32

	// Encrypted 为 false 时 data 里没有加密层（协议第一版）
	Encrypted bool

	// 请求/响应格式版本，语义见参考实现的 GamestatsRequestVersions /
	// GamestatsResponseVersions：
	//   请求 v1 不加密；v2 = 校验和 + pid；v3 = 校验和 + pid + 长度
	//   响应 v1 裸二进制；v2 末尾追加 40 字节十六进制 sha1
	RequestVersion  int
	ResponseVersion int
}

var (
	// Gen4GTS 是第四世代五款作品 (D/P/Pt/HG/SS) 的 GTS 与对战塔共用参数。
	// 与 mm201 的 pokemondpds.ashx.cs 构造参数完全一致。
	Gen4GTS = GtsCrypto{
		Salt:            "sAdeqWo3voLeC5r16DYv",
		RngMul:          0x45,
		RngAdd:          0x1111,
		RngMod:          0x80000000,
		ChecksumMask:    0x4a3b2c1d,
		Encrypted:       true,
		RequestVersion:  2,
		ResponseVersion: 1,
	}

	// Gen4Plaza 是 Pt/HG/SS 的 Wi-Fi Plaza (/pokemondpds/web/enc/lobby/...)。
	// 路径前缀与 GTS 相同但参数不同，这里只登记备用，尚未实现任何 handler。
	Gen4Plaza = GtsCrypto{
		Salt:            "uLMOGEiiJogofchScpXb",
		RngMul:          0x244fd,
		RngAdd:          0x6015,
		RngMod:          0x10000000,
		ChecksumMask:    0x5b440e7e,
		Encrypted:       true,
		RequestVersion:  3,
		ResponseVersion: 2,
	}
)

// VerifyHash 校验 &hash= 是否等于 sha1(Salt + token)。
func (c GtsCrypto) VerifyHash(hash string, token string) bool {
	hasher := sha1.New()
	hasher.Write([]byte(c.Salt))
	hasher.Write([]byte(token))
	return strings.EqualFold(hash, hex.EncodeToString(hasher.Sum(nil)))
}

// DecodeRequest 解开 &data=，返回负载里携带的 pid 与请求数据
// （请求数据不含前 4 字节校验和，也不含 4 字节 pid）。
func (c GtsCrypto) DecodeRequest(data string) (int32, []byte, error) {
	raw, err := fromURLSafeBase64(data)
	if err != nil {
		return 0, nil, fmt.Errorf("invalid base64 data: %w", err)
	}

	if !c.Encrypted {
		// 协议第一版：data 就是原始 pid + 负载
		if len(raw) < 4 {
			return 0, nil, errors.New("data too short to contain a pid")
		}
		return int32(binary.LittleEndian.Uint32(raw[0:4])), raw[4:], nil
	}

	// 版本 2/3 至少要有 4 字节校验和 + 4 字节 pid
	if len(raw) < 8 {
		return 0, nil, errors.New("data too short to contain a checksum and pid")
	}

	// 前 4 字节是大端存放的"校验和 ^ ChecksumMask"
	checksum := int32(binary.BigEndian.Uint32(raw[0:4])) ^ int32(c.ChecksumMask)

	// 种子由校验和的低 16 位与高 16 位拼成
	rand := uint32(checksum&0xFFFF) | uint32(checksum)<<16

	plain := make([]byte, len(raw)-4)
	var sum int32
	for i, b := range raw[4:] {
		rand = (rand*c.RngMul + c.RngAdd) % c.RngMod
		plain[i] = b ^ byte(rand>>16)
		sum += int32(plain[i])
	}

	// 校验和本身是明文各字节之和，解开后必须自洽
	if sum != checksum {
		return 0, nil, fmt.Errorf("data checksum mismatch: got %d, want %d", sum, checksum)
	}

	pid := int32(binary.LittleEndian.Uint32(plain[0:4]))
	return pid, plain[4:], nil
}

// ResponseChecksum 计算响应 v2 需要的尾部校验和：
// sha1(Salt + urlsafe-base64(响应体) + Salt) 的十六进制字符串。
// 第四世代 GTS 使用响应 v1，用不到这个函数，留作实现对战塔/后续世代时使用。
func (c GtsCrypto) ResponseChecksum(response []byte) string {
	hasher := sha1.New()
	hasher.Write([]byte(c.Salt))
	hasher.Write([]byte(base64.URLEncoding.EncodeToString(response)))
	hasher.Write([]byte(c.Salt))
	return hex.EncodeToString(hasher.Sum(nil))
}

// fromURLSafeBase64 解开 urlsafe base64。主机偶尔会省略 '=' 填充。
func fromURLSafeBase64(data string) ([]byte, error) {
	if pad := len(data) % 4; pad != 0 {
		data += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(data)
}
