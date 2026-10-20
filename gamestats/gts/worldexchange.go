package gts

// 第四世代宝可梦 GTS / 对战塔的 /pokemondpds/ 处理器。
//
// 这些接口都是 DS 游戏在 gamestats2 上访问的，特点是：
//   - 一对"挑战 + 应答"请求（第一次不带 hash 拿 token，第二次带 hash 与 data）；
//   - 响应必须是裸二进制，长度就是游戏要读的字节数，不能有任何填充或尾部哈希。
//
// 本文件负责把协议层面的流程跑通：每个接口都返回官方语义的响应码，
// 记录持久化与反作弊交给 GtsStore 的实现和后续 TODO。
//
// 参考: mm201/pkmn-classic-framework 的 gts/pokemondpds.ashx.cs

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"wwfc/logging"

	"github.com/logrusorgru/aurora/v3"
)

// PokemonGameStatsName 是第四世代宝可梦在 game_list.tsv 里的 game 名。
// 除它之外的 game 名不受本模块影响。
const PokemonGameStatsName = "pokemondpds"

// ---------------------------------------------------------------------------
// 存储后端
// ---------------------------------------------------------------------------

// GtsStore 是 GTS 记录的存储后端。骨架里用内存实现，接 PostgreSQL 时按同样的
// 接口实现一份替换 GtsStoreDefault 即可（建表风格参考仓库根目录的 schema.sql）。
type GtsStore interface {
	// DataForUser 返回该 pid 名下寄放的记录，没有则返回 nil。
	DataForUser(pid int32) *GtsRecord
	// Deposit 保存一条寄放记录。
	Deposit(record *GtsRecord) bool
	// Delete 删除该 pid 名下的记录。
	Delete(pid int32) bool
	// Search 按搜索条件查出至多 count 条记录。
	Search(pid int32, species uint16, gender uint8, minLevel uint8, maxLevel uint8, country uint8, count int) []*GtsRecord
	// SetLastSearch / LastSearch 记录玩家最近一次搜索的时间，
	// exchange.asp 用它判断玩家看到的是不是过期列表。
	SetLastSearch(pid int32)
	LastSearch(pid int32) (uint64, bool)
	// Lock 把 pid 名下的记录锁给 byPID 做交换，避免两个人同时抢同一只。
	Lock(pid int32, byPID int32) bool
	// CheckLock 判断 pid 名下的记录是否可以被 byPID 操作。
	CheckLock(pid int32, byPID int32) bool
	// Unlock 释放锁。
	Unlock(pid int32)
}

// GtsStoreDefault 是默认存储后端，进程内内存实现。
// 重启会丢数据，只用于把协议跑通。TODO: 换成 PostgreSQL。
var GtsStoreDefault GtsStore = newMemoryGtsStore()

type memoryGtsStore struct {
	mutex      sync.Mutex
	records    map[int32]*GtsRecord
	lastSearch map[int32]uint64
	locks      map[int32]int32
}

func newMemoryGtsStore() *memoryGtsStore {
	return &memoryGtsStore{
		records:    make(map[int32]*GtsRecord),
		lastSearch: make(map[int32]uint64),
		locks:      make(map[int32]int32),
	}
}

func (s *memoryGtsStore) DataForUser(pid int32) *GtsRecord {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	record := s.records[pid]
	if record == nil {
		return nil
	}
	return record.Clone()
}

func (s *memoryGtsStore) Deposit(record *GtsRecord) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.records[record.PID] = record.Clone()
	return true
}

func (s *memoryGtsStore) Delete(pid int32) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.records[pid] == nil {
		return false
	}
	delete(s.records, pid)
	delete(s.locks, pid)
	return true
}

func (s *memoryGtsStore) Search(pid int32, species uint16, gender uint8, minLevel uint8, maxLevel uint8, country uint8, count int) []*GtsRecord {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	var results []*GtsRecord
	for ownerPID, record := range s.records {
		if ownerPID == pid || record.IsExchanged != 0 {
			continue
		}
		if record.Species != species {
			continue
		}
		if !genderMatches(gender, record.Gender) {
			continue
		}
		if !CheckLevels(minLevel, maxLevel, record.Level) {
			continue
		}
		if country != 0 && record.TrainerCountry != country {
			continue
		}

		results = append(results, record.Clone())
	}

	// 参考实现是 ORDER BY TimeDeposited DESC，这里照做，保证玩家先看到最新的。
	sort.Slice(results, func(i, j int) bool {
		if results[i].TimeDeposited != results[j].TimeDeposited {
			return results[i].TimeDeposited > results[j].TimeDeposited
		}
		return results[i].PID < results[j].PID
	})

	if len(results) > count {
		results = results[:count]
	}
	return results
}

// Summary 输出存储里每一条记录的一行摘要，仅供排查"搜不到"时打日志用。
func (s *memoryGtsStore) Summary() []string {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	lines := make([]string, 0, len(s.records))
	for pid, record := range s.records {
		lines = append(lines, fmt.Sprintf("pid=%d %s", pid, record.Describe()))
	}
	sort.Strings(lines)
	return lines
}

// Explain 逐条说明存储里的记录为什么没有被这次搜索命中，仅用于排查。
func (s *memoryGtsStore) Explain(pid int32, species uint16, gender uint8, minLevel uint8, maxLevel uint8, country uint8) []string {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	lines := make([]string, 0, len(s.records))
	for ownerPID, record := range s.records {
		var reasons []string

		if ownerPID == pid {
			reasons = append(reasons, "pid 与搜索者相同：自己寄放的不出现在搜索结果里")
		}
		if record.IsExchanged != 0 {
			reasons = append(reasons, "已被换走，等原主人领取")
		}
		if record.Species != species {
			reasons = append(reasons, fmt.Sprintf("种族 %d != 搜索的 %d", record.Species, species))
		}
		if !genderMatches(gender, record.Gender) {
			reasons = append(reasons, fmt.Sprintf("性别 %d 不匹配搜索的 %d", record.Gender, gender))
		}
		if !CheckLevels(minLevel, maxLevel, record.Level) {
			reasons = append(reasons, fmt.Sprintf("等级 %d 不在 %d-%d", record.Level, minLevel, maxLevel))
		}
		if country != 0 && record.TrainerCountry != country {
			reasons = append(reasons, fmt.Sprintf("国家 %d != 搜索的 %d", record.TrainerCountry, country))
		}
		if len(reasons) == 0 {
			reasons = append(reasons, "本应命中")
		}

		lines = append(lines, fmt.Sprintf("pid=%d %s <- %s", ownerPID, record.Describe(), strings.Join(reasons, "，")))
	}
	sort.Strings(lines)
	return lines
}

func (s *memoryGtsStore) SetLastSearch(pid int32) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.lastSearch[pid] = TimeToGtsTimestamp(time.Now().UTC())
}

func (s *memoryGtsStore) LastSearch(pid int32) (uint64, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	value, ok := s.lastSearch[pid]
	return value, ok
}

func (s *memoryGtsStore) Lock(pid int32, byPID int32) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	owner, locked := s.locks[pid]
	if locked && owner != byPID {
		return false
	}
	s.locks[pid] = byPID
	return true
}

func (s *memoryGtsStore) CheckLock(pid int32, byPID int32) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	owner, locked := s.locks[pid]
	return !locked || owner == byPID
}

func (s *memoryGtsStore) Unlock(pid int32) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	delete(s.locks, pid)
}

// ---------------------------------------------------------------------------
// 跨请求状态
// ---------------------------------------------------------------------------

// gtsPendingOp 保存 post.asp / exchange.asp 与它们的 _finish 之间的中间状态。
// 这一对请求之间隔着一次存档，所以状态必须放在服务端。
type gtsPendingOp struct {
	record    *GtsRecord // post.asp 上传的记录
	target    *GtsRecord // exchange.asp 选中要换过来的记录
	targetPID int32
	createdAt time.Time
}

var (
	gtsPendingMutex sync.Mutex
	gtsPendingOps   = map[int32]*gtsPendingOp{}
)

// pendingOpTTL 是中间状态的存活时间，超时后丢弃，避免玩家中途断开导致内存泄漏。
const pendingOpTTL = 5 * time.Minute

func setPendingOp(pid int32, op *gtsPendingOp) {
	gtsPendingMutex.Lock()
	defer gtsPendingMutex.Unlock()

	op.createdAt = time.Now()
	gtsPendingOps[pid] = op
}

// takePendingOp 取出并清掉中间状态；超时或不存在时返回 nil。
func takePendingOp(moduleName string, pid int32, kind string) *gtsPendingOp {
	gtsPendingMutex.Lock()
	defer gtsPendingMutex.Unlock()

	op := gtsPendingOps[pid]
	if op == nil {
		return nil
	}
	delete(gtsPendingOps, pid)

	if time.Since(op.createdAt) > pendingOpTTL {
		logging.Warn(moduleName, "Discarding expired", aurora.Cyan(kind), "state for pid", aurora.Cyan(pid))
		return nil
	}
	return op
}

// ---------------------------------------------------------------------------
// 入口
// ---------------------------------------------------------------------------

// shortResponse 生成 2 字节小端响应码，例如 0x0005 -> "05 00"。
func shortResponse(value uint16) []byte {
	return []byte{byte(value), byte(value >> 8)}
}

// gtsStoreInspector 是存储后端的可选接口。内存实现提供可读摘要与"为什么没命中"
// 的说明，供搜索结果为 0 时打日志用；换成数据库实现时可以不实现它。
type gtsStoreInspector interface {
	Summary() []string
	Explain(pid int32, species uint16, gender uint8, minLevel uint8, maxLevel uint8, country uint8) []string
}

// logGtsStoreContents 把存储里的记录逐条打出来，并说明这次搜索为什么没命中，
// 方便一眼判断问题出在寄放（压根没入库）还是筛选条件。
func logGtsStoreContents(moduleName string, pid int32, species uint16, gender uint8, minLevel uint8, maxLevel uint8, country uint8) {
	inspector, ok := GtsStoreDefault.(gtsStoreInspector)
	if !ok {
		return
	}

	if len(inspector.Summary()) == 0 {
		logging.Warn(moduleName, "GTS store is empty — nothing was ever deposited (check post_finish.asp)")
		return
	}

	for _, line := range inspector.Explain(pid, species, gender, minLevel, maxLevel, country) {
		logging.Info(moduleName, "  ", line)
	}
}

// IsPokemonGameStatsPath 判断这次 gamestats 请求是不是第四世代宝可梦的。
// web.go 用它决定要不要把请求交给本模块（从而跳过 DWC padding 与尾部哈希）。
func IsPokemonGameStatsPath(gameName string) bool {
	return gameName == PokemonGameStatsName
}

// HTTPError 表示"响应还没有写出去，调用方需要自己回一个错误页"。
// 用它而不是裸的 (status, msg) 是为了让"已经写好二进制响应"和"需要写错误页"
// 在类型上互斥——避免成功响应写完之后又被追加一个错误页。
type HTTPError struct {
	Status int
	Text   string
}

func (e *HTTPError) Error() string { return e.Text }

// HandlePokemonGameStats 处理一次"带 hash"的宝可梦 gamestats 请求。
// 成功时直接写出裸二进制响应并返回 nil；失败时什么都不写，返回 *HTTPError
// 交给调用方（web.go）统一回错误页。
//
// 响应必须是裸二进制：只有 Content-Length 一个头有意义，DS 不看别的头，
// 也不能有 DWC padding 或尾部哈希。
func HandlePokemonGameStats(w http.ResponseWriter, subPath string, query url.Values, token string, moduleName string) *HTTPError {
	pidValue, err := strconv.ParseInt(query.Get("pid"), 10, 32)
	if err != nil {
		logging.Warn(moduleName, "Missing or invalid pid:", aurora.Cyan(query.Get("pid")))
		return &HTTPError{http.StatusBadRequest, "400 Bad Request"}
	}
	pid := int32(pidValue)

	if !Gen4GTS.VerifyHash(query.Get("hash"), token) {
		// 正常流程下 hash 一定对得上（token 是由同一个 pid 的第一次请求换来的），
		// 对不上说明不是我们发出去的挑战值，直接拒掉。
		logging.Warn(moduleName, "Invalid gamestats hash")
		return &HTTPError{http.StatusBadRequest, "400 Bad Request"}
	}

	payloadPID, payload, err := Gen4GTS.DecodeRequest(query.Get("data"))
	if err != nil {
		logging.Warn(moduleName, "Failed to decode gamestats request:", err.Error())
		return &HTTPError{http.StatusBadRequest, "400 Bad Request"}
	}

	if payloadPID != pid {
		logging.Warn(moduleName, "Pid in payload does not match query:", aurora.Cyan(payloadPID), "!=", aurora.Cyan(pid))
		return &HTTPError{http.StatusBadRequest, "400 Bad Request"}
	}

	response, status, handled := handlePokemonRequest(subPath, pid, payload, moduleName)
	if !handled {
		logging.Warn(moduleName, "Unhandled path:", aurora.Cyan(subPath))
		return &HTTPError{http.StatusNotFound, "404 Not Found"}
	}

	if status != 0 {
		return &HTTPError{status, fmt.Sprintf("%d %s", status, http.StatusText(status))}
	}

	writeBinaryResponse(w, response)

	return nil
}

// writeBinaryResponse 写一个不带任何填充的二进制响应。
func writeBinaryResponse(w http.ResponseWriter, response []byte) {
	w.Header().Set("Content-type", "application/octet-stream")
	w.Header().Set("Server", "Microsoft-IIS/6.0")
	w.Header().Add("Server", "GSTPRDSTATSWEB2")
	w.Header().Set("X-Powered-By", "ASP.NET")
	w.Header().Set("Content-Length", strconv.Itoa(len(response)))
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(response); err != nil {
		logging.Error("GSTATS", "Error writing response:", err)
	}
}

// handlePokemonRequest 处理 /pokemondpds/ 下的请求。
// path 是去掉域名后的路径（可以带也可以不带 /pokemondpds 前缀），
// data 是解密后的请求负载（不含校验和与 pid）。
//
// 返回 (响应体, HTTP 状态码, 是否已处理)；状态码为 0 表示 200。
// 返回的响应体已经是严格按协议拼好的裸二进制，调用方不要再做任何加工。
func handlePokemonRequest(path string, pid int32, data []byte, moduleName string) ([]byte, int, bool) {
	if strings.HasPrefix(path, "/pokemondpds") {
		path = strings.TrimPrefix(path, "/pokemondpds")
	}

	switch path {
	// 启动时上报训练家资料。D/P 不调用它，Pt/HG/SS 在 info.asp 之后必调。
	// 官方对 GTS 场景的应答是 8 个 0。
	case "/common/setProfile.asp":
		if len(data) != 100 {
			logging.Warn(moduleName, "setProfile.asp: unexpected payload length", aurora.Cyan(len(data)))
			return nil, http.StatusBadRequest, true
		}

		// TODO: 解析 TrainerProfile4 并入库（昵称、国家、Wii 邮件地址等）
		return make([]byte, 8), 0, true

	// GTS 是否在线的探测。0x0001 表示一切正常。
	case "/worldexchange/info.asp":
		if len(data) != 0 {
			logging.Warn(moduleName, "info.asp: unexpected payload length", aurora.Cyan(len(data)))
		}
		return shortResponse(1), 0, true

	// 启动时以及查看自己寄放的宝可梦状态时调用。
	case "/worldexchange/result.asp":
		record := GtsStoreDefault.DataForUser(pid)
		switch {
		case record == nil:
			return shortResponse(5), 0, true // 系统里没有你的宝可梦
		case record.IsExchanged != 0:
			return record.Encode(), 0, true // 有人换给了你，把这只递过去
		default:
			return shortResponse(4), 0, true // 你的宝可梦还在，还没被换走
		}

	// 查看寄放详情 / 取回前的确认。
	case "/worldexchange/get.asp":
		record := GtsStoreDefault.DataForUser(pid)
		if record == nil {
			return shortResponse(5), 0, true
		}
		return record.Encode(), 0, true

	// 收到别人换给你的宝可梦之后，把它从系统里删掉。
	case "/worldexchange/delete.asp":
		record := GtsStoreDefault.DataForUser(pid)
		if record == nil {
			return shortResponse(3), 0, true
		}
		if record.IsExchanged == 0 {
			// 系统里是你自己寄放的那只，应该走 return.asp
			return shortResponse(3), 0, true
		}
		if !GtsStoreDefault.Delete(pid) {
			return shortResponse(5), 0, true
		}
		// TODO: 在这里记一笔成功交换的日志
		return shortResponse(1), 0, true

	// 取回自己寄放、还没被换走的宝可梦。
	case "/worldexchange/return.asp":
		record := GtsStoreDefault.DataForUser(pid)
		if record == nil || record.IsExchanged != 0 || !GtsStoreDefault.CheckLock(pid, pid) {
			return shortResponse(2), 0, true
		}
		if !GtsStoreDefault.Delete(pid) {
			return shortResponse(2), 0, true
		}
		return shortResponse(1), 0, true

	// 寄放一只宝可梦。此时游戏还没存档，真正的入库要等 post_finish.asp。
	case "/worldexchange/post.asp":
		if len(data) != GtsRecordSize {
			logging.Warn(moduleName, "post.asp: unexpected payload length", aurora.Cyan(len(data)))
			return nil, http.StatusBadRequest, true
		}

		if GtsStoreDefault.DataForUser(pid) != nil {
			// 系统里已经有一只了，把玩家顶出去让他重新查状态
			return shortResponse(0x0E), 0, true
		}

		record, err := DecodeGtsRecord(data)
		if err != nil {
			logging.Error(moduleName, "post.asp: failed to decode record:", err.Error())
			return nil, http.StatusBadRequest, true
		}

		record.IsExchanged = 0
		logging.Info(moduleName, "GTS deposit request from", aurora.Cyan(pid), ":", record.Describe())
		if !record.Validate() {
			// 0x0C = "这只宝可梦不能拿出来交换"
			logging.Warn(moduleName, "GTS deposit rejected by validation:", record.Describe())
			return shortResponse(0x0C), 0, true
		}

		// 这两个字段游戏上传时是空的，必须由服务端填。
		record.TimeDeposited = TimeToGtsTimestamp(time.Now().UTC())
		record.TimeExchanged = 0
		record.PID = pid

		setPendingOp(pid, &gtsPendingOp{record: record})
		return shortResponse(1), 0, true

	// 游戏存档完成，把 post.asp 暂存的记录真正入库。
	case "/worldexchange/post_finish.asp":
		if len(data) != 8 {
			logging.Warn(moduleName, "post_finish.asp: unexpected payload length", aurora.Cyan(len(data)))
			return nil, http.StatusBadRequest, true
		}

		op := takePendingOp(moduleName, pid, "post_finish.asp")
		if op == nil || op.record == nil {
			logging.Warn(moduleName, "post_finish.asp: no pending deposit for pid", aurora.Cyan(pid))
			return shortResponse(2), 0, true
		}
		if !GtsStoreDefault.Deposit(op.record) {
			logging.Error(moduleName, "post_finish.asp: store refused the record for pid", aurora.Cyan(pid))
			return shortResponse(5), 0, true
		}
		logging.Info(moduleName, "GTS deposit committed:", aurora.Cyan(pid), "->", op.record.Describe())
		return shortResponse(1), 0, true

	// 搜索别人寄放的宝可梦，响应是若干条 292 字节记录首尾相接。
	case "/worldexchange/search.asp":
		if len(data) < 7 || len(data) > 8 {
			logging.Warn(moduleName, "search.asp: unexpected payload length", aurora.Cyan(len(data)))
			return nil, http.StatusBadRequest, true
		}

		species := binary.LittleEndian.Uint16(data[0:])
		if species == 0 {
			return nil, http.StatusBadRequest, true
		}

		gender := data[2]
		minLevel := data[3]
		maxLevel := data[4]
		// data[5] 含义未知
		resultsCount := int(data[6])
		var country uint8
		if len(data) > 7 {
			country = data[7]
		}

		if resultsCount < 1 {
			// 玩家本来就没要结果，直接回空
			return nil, 0, true
		}
		if resultsCount > 7 {
			resultsCount = 7
		}

		records := GtsStoreDefault.Search(pid, species, gender, minLevel, maxLevel, country, resultsCount)
		response := make([]byte, 0, len(records)*GtsRecordSize)
		for _, record := range records {
			response = append(response, record.Encode()...)
		}

		logging.Info(moduleName, "GTS search:",
			"species", aurora.Cyan(species),
			"gender", aurora.Cyan(gender),
			"level", aurora.Cyan(fmt.Sprintf("%d-%d", minLevel, maxLevel)),
			"country", aurora.Cyan(country),
			"want", aurora.Cyan(resultsCount),
			"->", aurora.Cyan(len(records)), "record(s)")
		if len(records) == 0 {
			// 搜不到时把存储里的东西全打出来，并说明每条为什么没命中，
			// 一眼就能看出是"没存进去"还是"被筛选条件挡掉了"。
			logGtsStoreContents(moduleName, pid, species, gender, minLevel, maxLevel, country)
		}

		GtsStoreDefault.SetLastSearch(pid)
		return response, 0, true

	// 发起交换：292 字节记录 + 末尾 4 字节"想换的对手 pid"。
	case "/worldexchange/exchange.asp":
		if len(data) != GtsRecordSize+4 {
			logging.Warn(moduleName, "exchange.asp: unexpected payload length", aurora.Cyan(len(data)))
			return nil, http.StatusBadRequest, true
		}

		upload, err := DecodeGtsRecord(data[:GtsRecordSize])
		if err != nil {
			logging.Error(moduleName, "exchange.asp: failed to decode record:", err.Error())
			return nil, http.StatusBadRequest, true
		}
		upload.IsExchanged = 0
		targetPID := int32(binary.LittleEndian.Uint32(data[GtsRecordSize:]))

		target := GtsStoreDefault.DataForUser(targetPID)
		lastSearch, searched := GtsStoreDefault.LastSearch(pid)

		// 玩家搜索之后目标被换走或取回 → 他看到的是过期列表
		if target == nil || !searched ||
			target.TimeDeposited > lastSearch ||
			target.IsExchanged != 0 {
			return shortResponse(2), 0, true
		}

		if !upload.Validate() || !upload.CanTrade(target) {
			return shortResponse(0x0C), 0, true
		}

		if !GtsStoreDefault.Lock(targetPID, pid) {
			// 晚了一步，已经被人换走
			return shortResponse(2), 0, true
		}

		setPendingOp(pid, &gtsPendingOp{record: upload, target: target, targetPID: targetPID})

		// 直接把对手那条记录原样返回，客户端据此把宝可梦存进自己的游戏。
		return target.Encode(), 0, true

	// 交换完成（玩家已存档）。
	case "/worldexchange/exchange_finish.asp":
		if len(data) != 8 {
			logging.Warn(moduleName, "exchange_finish.asp: unexpected payload length", aurora.Cyan(len(data)))
			return nil, http.StatusBadRequest, true
		}

		op := takePendingOp(moduleName, pid, "exchange_finish.asp")
		if op == nil || op.record == nil || op.targetPID == 0 {
			return shortResponse(2), 0, true
		}

		// 交换成立：
		//   - 发起者(pid)名下的记录清空：他的宝可梦已经交出去，换来的那只本地已经存好；
		//   - 对手(op.targetPID)名下换成发起者上传的那只，标记 IsExchanged，
		//     等对手下次进 GTS 时由 result.asp 领走，再由 delete.asp 清除。
		// TODO: 这两步要放进同一个数据库事务；这里的内存实现没有原子性保证。
		GtsStoreDefault.Delete(pid)

		incoming := op.record.Clone()
		incoming.PID = op.targetPID
		incoming.TimeDeposited = op.record.TimeDeposited
		incoming.TimeExchanged = TimeToGtsTimestamp(time.Now().UTC())
		incoming.IsExchanged = 0x01

		GtsStoreDefault.Deposit(incoming)
		GtsStoreDefault.Unlock(op.targetPID)

		logging.Info(moduleName, "Trade committed:", aurora.Cyan(pid), "->", aurora.Cyan(op.targetPID))
		return shortResponse(1), 0, true

	// 对战塔（尚未实现）
	case "/battletower/info.asp",
		"/battletower/roomnum.asp",
		"/battletower/download.asp",
		"/battletower/upload.asp":
		logging.Warn(moduleName, "Battletower path is not implemented yet:", aurora.Cyan(path))
		return nil, http.StatusNotFound, true

	// Wi-Fi Plaza 等其它 /pokemondpds/ 子路径
	default:
		return nil, 0, false
	}
}
