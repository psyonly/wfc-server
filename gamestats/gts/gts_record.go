package gts

// 第四世代 GTS 里的一条"寄放记录"，定长 292 (0x124) 字节。
// 布局来自 mm201/pkmn-classic-framework 的 library/Wfc/GtsRecord4.cs。
//
//	0x0000  bytes[236] 混淆过的 PKM 数据 (party 格式)
//	0x00EC  uint16     Species
//	0x00EE  byte       Gender
//	0x00EF  byte       Level
//	0x00F0  uint16     RequestedSpecies
//	0x00F2  byte       RequestedGender
//	0x00F3  byte       RequestedMinLevel
//	0x00F4  byte       RequestedMaxLevel
//	0x00F5  byte       未知
//	0x00F6  byte       TrainerGender
//	0x00F7  byte       未知
//	0x00F8  uint64     寄放时间
//	0x0100  uint64     换出时间
//	0x0108  int32      PID
//	0x010C  bytes[16]  训练家名 (Gen4 编码)
//	0x011C  uint16     训练家 OT
//	0x011E  byte       TrainerCountry
//	0x011F  byte       TrainerRegion
//	0x0120  byte       TrainerClass
//	0x0121  byte       IsExchanged
//	0x0122  byte       TrainerVersion
//	0x0123  byte       TrainerLanguage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// GtsRecordSize 是一条 GTS 记录的字节长度。
const GtsRecordSize = 292

// 性别字段的取值。注意 DS 端用的不是 PKM 里的 0/1/2，而是
// 1=雄、2=雌、3=不分性别/不限（Both "Either" 和 "None" 都是 3）。
// 参考 mm201/pkmn-classic-framework 的 library/Structures/Enums.cs:
//
//	Male = 1, Female = 2, None = 3, Either = 3
const (
	GenderMale   uint8 = 1
	GenderFemale uint8 = 2
	GenderEither uint8 = 3
)

// genderMatches 判断一条记录的性别是否满足搜索条件。
// 搜索选"不限"时不筛；否则命中同性别，另外也放过 genderless 的记录
// （参考实现的 SQL 是 Gender IN (@gender, 3)，这里保持一致）。
func genderMatches(searchGender uint8, recordGender uint8) bool {
	if searchGender == GenderEither {
		return true
	}
	return recordGender == searchGender || recordGender == GenderEither
}

// GtsRecord 是解码后的 GTS 记录。
type GtsRecord struct {
	PokemonData [0xEC]byte

	Species           uint16
	Gender            uint8
	Level             uint8
	RequestedSpecies  uint16
	RequestedGender   uint8
	RequestedMinLevel uint8
	RequestedMaxLevel uint8
	Unknown1          uint8
	TrainerGender     uint8
	Unknown2          uint8
	TimeDeposited     uint64
	TimeExchanged     uint64
	PID               int32
	TrainerName       [16]byte
	TrainerOT         uint16
	TrainerCountry    uint8
	TrainerRegion     uint8
	TrainerClass      uint8
	IsExchanged       uint8
	TrainerVersion    uint8
	TrainerLanguage   uint8
}

// DecodeGtsRecord 解析一条 292 字节记录。
func DecodeGtsRecord(data []byte) (*GtsRecord, error) {
	if len(data) != GtsRecordSize {
		return nil, errors.New("gts record must be 292 bytes")
	}

	record := &GtsRecord{}
	copy(record.PokemonData[:], data[0x0000:0x00EC])
	record.Species = binary.LittleEndian.Uint16(data[0x00EC:])
	record.Gender = data[0x00EE]
	record.Level = data[0x00EF]
	record.RequestedSpecies = binary.LittleEndian.Uint16(data[0x00F0:])
	record.RequestedGender = data[0x00F2]
	record.RequestedMinLevel = data[0x00F3]
	record.RequestedMaxLevel = data[0x00F4]
	record.Unknown1 = data[0x00F5]
	record.TrainerGender = data[0x00F6]
	record.Unknown2 = data[0x00F7]
	record.TimeDeposited = binary.LittleEndian.Uint64(data[0x00F8:])
	record.TimeExchanged = binary.LittleEndian.Uint64(data[0x0100:])
	record.PID = int32(binary.LittleEndian.Uint32(data[0x0108:]))
	copy(record.TrainerName[:], data[0x010C:0x011C])
	record.TrainerOT = binary.LittleEndian.Uint16(data[0x011C:])
	record.TrainerCountry = data[0x011E]
	record.TrainerRegion = data[0x011F]
	record.TrainerClass = data[0x0120]
	record.IsExchanged = data[0x0121]
	record.TrainerVersion = data[0x0122]
	record.TrainerLanguage = data[0x0123]

	return record, nil
}

// Encode 把记录序列化回 292 字节。
func (r *GtsRecord) Encode() []byte {
	data := make([]byte, GtsRecordSize)
	copy(data[0x0000:], r.PokemonData[:])
	binary.LittleEndian.PutUint16(data[0x00EC:], r.Species)
	data[0x00EE] = r.Gender
	data[0x00EF] = r.Level
	binary.LittleEndian.PutUint16(data[0x00F0:], r.RequestedSpecies)
	data[0x00F2] = r.RequestedGender
	data[0x00F3] = r.RequestedMinLevel
	data[0x00F4] = r.RequestedMaxLevel
	data[0x00F5] = r.Unknown1
	data[0x00F6] = r.TrainerGender
	data[0x00F7] = r.Unknown2
	binary.LittleEndian.PutUint64(data[0x00F8:], r.TimeDeposited)
	binary.LittleEndian.PutUint64(data[0x0100:], r.TimeExchanged)
	binary.LittleEndian.PutUint32(data[0x0108:], uint32(r.PID))
	copy(data[0x010C:], r.TrainerName[:])
	binary.LittleEndian.PutUint16(data[0x011C:], r.TrainerOT)
	data[0x011E] = r.TrainerCountry
	data[0x011F] = r.TrainerRegion
	data[0x0120] = r.TrainerClass
	data[0x0121] = r.IsExchanged
	data[0x0122] = r.TrainerVersion
	data[0x0123] = r.TrainerLanguage
	return data
}

// Clone 复制一条记录。
func (r *GtsRecord) Clone() *GtsRecord {
	clone := *r
	return &clone
}

// Describe 输出一行人类可读的摘要，只用于日志排查。
func (r *GtsRecord) Describe() string {
	return fmt.Sprintf("species=%d gender=%d lv=%d wants=%d gender=%d lv=%d-%d pid=%d country=%d ot=%d exchanged=%d",
		r.Species, r.Gender, r.Level,
		r.RequestedSpecies, r.RequestedGender, r.RequestedMinLevel, r.RequestedMaxLevel,
		r.PID, r.TrainerCountry, r.TrainerOT, r.IsExchanged)
}

// CheckLevels 判断等级是否落在 [min, max] 之间；max 为 0 表示不限上限。
func CheckLevels(min uint8, max uint8, level uint8) bool {
	if max == 0 {
		max = 0xFF
	}
	return level >= min && level <= max
}

// Validate 做基础的合法性检查。
// TODO: 这里只覆盖了服务器侧必须拦住的几种明显问题。参考实现还会校验
// PKM 结构本身（校验和、种族值上限、EV 总和 ≤ 510、蛋/坏蛋、胶囊等），
// 需要时再补 PokemonData 的解密与解析。
func (r *GtsRecord) Validate() bool {
	if r.Species == 0 || r.RequestedSpecies == 0 {
		return false
	}
	if r.Level == 0 || r.Level > 100 {
		return false
	}
	if r.RequestedMinLevel > 100 || r.RequestedMaxLevel > 100 {
		return false
	}
	if r.RequestedMinLevel != 0 && r.RequestedMaxLevel != 0 && r.RequestedMinLevel > r.RequestedMaxLevel {
		return false
	}
	return true
}

// CanTrade 判断两条记录是否互相满足对方的交换条件。
func (r *GtsRecord) CanTrade(other *GtsRecord) bool {
	if r.IsExchanged != 0 || other.IsExchanged != 0 {
		return false
	}

	if r.Species != other.RequestedSpecies {
		return false
	}
	if other.RequestedGender != GenderEither && r.Gender != other.RequestedGender {
		return false
	}
	if !CheckLevels(other.RequestedMinLevel, other.RequestedMaxLevel, r.Level) {
		return false
	}

	if other.Species != r.RequestedSpecies {
		return false
	}
	if r.RequestedGender != GenderEither && other.Gender != r.RequestedGender {
		return false
	}
	if !CheckLevels(r.RequestedMinLevel, r.RequestedMaxLevel, other.Level) {
		return false
	}

	return true
}

// FlagTraded 把目标记录标记为"已被换走"，并填入换出的宝可梦信息。
// 这一步只是为了生成 exchange.asp 要返回给玩家的记录。
func (r *GtsRecord) FlagTraded(other *GtsRecord) {
	r.Species = other.Species
	r.Gender = other.Gender
	r.Level = other.Level
	r.RequestedSpecies = other.RequestedSpecies
	r.RequestedGender = other.RequestedGender
	r.RequestedMinLevel = other.RequestedMinLevel
	r.RequestedMaxLevel = other.RequestedMaxLevel
	r.TimeDeposited = other.TimeDeposited
	r.TimeExchanged = TimeToGtsTimestamp(time.Now().UTC())
	r.PID = other.PID
	r.IsExchanged = 0x01
}

// TimeToGtsTimestamp 把时间打包成 GTS 使用的时间戳：
// 年放到 bit48、月 bit40、日 bit32、时 bit24、分 bit16、秒 bit8。
func TimeToGtsTimestamp(t time.Time) uint64 {
	return uint64(t.Year()&0xFFFF)<<0x30 |
		uint64(t.Month()&0xFF)<<0x28 |
		uint64(t.Day()&0xFF)<<0x20 |
		uint64(t.Hour()&0xFF)<<0x18 |
		uint64(t.Minute()&0xFF)<<0x10 |
		uint64(t.Second()&0xFF)<<0x08
}

// GtsTimestampToTime 是 TimeToGtsTimestamp 的逆运算，0 表示"没有时间"。
func GtsTimestampToTime(timestamp uint64) (time.Time, bool) {
	if timestamp == 0 {
		return time.Time{}, false
	}

	year := int(timestamp>>0x30) & 0xFFFF
	month := int(timestamp>>0x28) & 0xFF
	day := int(timestamp>>0x20) & 0xFF
	hour := int(timestamp>>0x18) & 0xFF
	minute := int(timestamp>>0x10) & 0xFF
	second := int(timestamp>>0x08) & 0xFF

	// 主机给的时间戳不一定合法，用 time.Date 归一化而不是 panic
	return time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC), true
}
