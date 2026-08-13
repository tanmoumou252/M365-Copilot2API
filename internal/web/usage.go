package web

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type UsageRecord struct {
	Time         time.Time `json:"time"`
	APIKeyPrefix string    `json:"api_key_prefix"`
	AccountEmail string    `json:"account_email"`
	Model        string    `json:"model"`
	Endpoint     string    `json:"endpoint"`
	Stream       bool      `json:"stream"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CacheTokens  int64     `json:"cache_tokens"`
	DurationMs   int64     `json:"duration_ms"`
	Status       int       `json:"status"`
}

const maxUsageRecords = 50000

type usageLog struct {
	mu        sync.Mutex
	Path      string
	records   []UsageRecord
	pending   []UsageRecord
	retention time.Duration // 0 表示关闭时间裁剪，仅保留 maxUsageRecords 计数上限
	persist   *persistStore
}

var globalUsage = &usageLog{}

func openUsageLog() *usageLog {
	p := strings.TrimSpace(os.Getenv("M365_USAGE_LOG"))
	if p == "" {
		dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR"))
		if dir == "" {
			h, _ := os.UserHomeDir()
			dir = filepath.Join(h, ".config", "m365-copilot2api")
		}
		p = filepath.Join(dir, "usage.jsonl")
	}
	s := &usageLog{Path: p, retention: parseRetentionHours()}
	s.persist = &persistStore{flush: s.flush}
	_ = os.MkdirAll(filepath.Dir(p), 0700)
	s.load()
	return s
}

// parseRetentionHours 解析 M365_USAGE_RETENTION_HOURS：
// 缺省或解析失败保持默认 2h；正整数 N → N·h；0 或负数 → 0（关闭时间裁剪）。
func parseRetentionHours() time.Duration {
	v := strings.TrimSpace(os.Getenv("M365_USAGE_RETENTION_HOURS"))
	if v == "" {
		return 2 * time.Hour
	}
	h, err := strconv.Atoi(v)
	if err != nil {
		return 2 * time.Hour
	}
	if h > 0 {
		return time.Duration(h) * time.Hour
	}
	return 0
}

func (s *usageLog) load() {
	f, err := os.Open(s.Path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lines int
	for scanner.Scan() {
		var rec UsageRecord
		if json.Unmarshal(scanner.Bytes(), &rec) == nil {
			s.records = append(s.records, rec)
			lines++
		}
	}
	s.trim()
	// 迁移标记：开启时间裁剪且 load 时发生裁剪（磁盘原始行数 > 裁剪后内存记录数），
	// 标记 dirty，使启动后首轮 persist tick 触发整文件重写，立即压缩历史大文件。
	// 否则旧大文件要等到首个新请求 record() 后才会被重写。
	if s.retention > 0 && len(s.records) < lines {
		s.persist.markDirty()
	}
}

func (s *usageLog) trim() {
	if s.retention > 0 {
		cutoff := time.Now().Add(-s.retention)
		i := 0
		for i < len(s.records) && s.records[i].Time.Before(cutoff) {
			i++
		}
		s.records = s.records[i:]
	}
	if len(s.records) > maxUsageRecords {
		s.records = s.records[len(s.records)-maxUsageRecords:]
	}
}

func (s *usageLog) record(rec UsageRecord) {
	s.mu.Lock()
	s.records = append(s.records, rec)
	s.trim()
	s.pending = append(s.pending, rec)
	s.mu.Unlock()
	s.persist.markDirty()
}

// flush 整文件重写：把当前已裁剪的 records 全量写回，保证磁盘文件 == 内存
// records，从根上给文件大小设上限（不再 append-only 无限增长）。失败仅 markDirty
// 让下一轮 persist tick 重试全量重写，内存 records 不丢失，不涉及 pending 回退。
func (s *usageLog) flush() error {
	s.mu.Lock()
	snap := append([]UsageRecord(nil), s.records...)
	s.pending = nil
	s.mu.Unlock()
	var buf []byte
	for _, rec := range snap {
		if b, err := json.Marshal(rec); err == nil {
			buf = append(buf, b...)
			buf = append(buf, '\n')
		}
	}
	if err := writeFileAtomic(s.Path, buf, 0600); err != nil {
		s.persist.markDirty()
		return err
	}
	return nil
}

// purgePrefix 从 records 与 pending 中移除所有 APIKeyPrefix == prefix 的记录，
// 随后 markDirty 触发重写落盘（删除 API Key 时联动清除其用量历史）。
func (s *usageLog) purgePrefix(prefix string) {
	if prefix == "" {
		return
	}
	s.mu.Lock()
	kept := s.records[:0]
	for _, r := range s.records {
		if r.APIKeyPrefix != prefix {
			kept = append(kept, r)
		}
	}
	s.records = kept
	keptP := s.pending[:0]
	for _, r := range s.pending {
		if r.APIKeyPrefix != prefix {
			keptP = append(keptP, r)
		}
	}
	s.pending = keptP
	s.mu.Unlock()
	s.persist.markDirty()
}

func (s *usageLog) snapshot(days int) map[string]any {
	s.mu.Lock()
	recs := append([]UsageRecord(nil), s.records...)
	s.mu.Unlock()

	// cutoff 仅决定从“已保留”记录中按时间过滤展示的跨度（days 面板）。
	// 真正的磁盘保留窗口由 M365_USAGE_RETENTION_HOURS 控制（见 trim）。
	cutoff := time.Now().AddDate(0, 0, -days)
	loc := time.Now().Location()
	today := time.Now().In(loc).Truncate(24 * time.Hour)
	dayAgo := time.Now().Add(-24 * time.Hour)

	var (
		requests, in, out, cache, durationMs int64
		todayReq, todayTok                   int64
		h24Req, h24Tok                       int64
	)
	keyCounts := map[string]*usageCountStat{}
	modelCounts := map[string]*usageCountStat{}
	endpointCounts := map[string]*usageCountStat{}
	trendMap := map[string]*usageTrendPoint{}

	for _, rec := range recs {
		if rec.Time.Before(cutoff) {
			continue
		}
		requests++
		reqTok := rec.InputTokens + rec.OutputTokens + rec.CacheTokens
		in += rec.InputTokens
		out += rec.OutputTokens
		cache += rec.CacheTokens
		durationMs += rec.DurationMs
		if rec.Time.After(today) {
			todayReq++
			todayTok += reqTok
		}
		if rec.Time.After(dayAgo) {
			h24Req++
			h24Tok += reqTok
		}
		key := rec.APIKeyPrefix
		ks, ok := keyCounts[key]
		if !ok {
			ks = &usageCountStat{}
			keyCounts[key] = ks
		}
		ks.Requests++
		ks.Tokens += reqTok
		if mc, ok := modelCounts[rec.Model]; ok {
			mc.Requests++
			mc.Tokens += reqTok
		} else {
			modelCounts[rec.Model] = &usageCountStat{Requests: 1, Tokens: reqTok}
		}
		if ec, ok := endpointCounts[rec.Endpoint]; ok {
			ec.Requests++
			ec.Tokens += reqTok
		} else {
			endpointCounts[rec.Endpoint] = &usageCountStat{Requests: 1, Tokens: reqTok}
		}
		date := rec.Time.In(loc).Format("01-02")
		if tp, ok := trendMap[date]; ok {
			tp.Requests++
			tp.Tokens += reqTok
		} else {
			trendMap[date] = &usageTrendPoint{Date: date, Requests: 1, Tokens: reqTok}
		}
	}

	avgMs := int64(0)
	if requests > 0 {
		avgMs = durationMs / requests
	}

	model := make([]map[string]any, 0, len(modelCounts))
	for name, c := range modelCounts {
		model = append(model, map[string]any{"name": name, "requests": c.Requests, "tokens": c.Tokens})
	}
	sort.Slice(model, func(i, j int) bool { return model[i]["tokens"].(int64) > model[j]["tokens"].(int64) })

	ep := make([]map[string]any, 0, len(endpointCounts))
	for k, c := range endpointCounts {
		ep = append(ep, map[string]any{"endpoint": k, "requests": c.Requests, "tokens": c.Tokens})
	}
	sort.Slice(ep, func(i, j int) bool { return ep[i]["tokens"].(int64) > ep[j]["tokens"].(int64) })

	keys := make([]map[string]any, 0, len(keyCounts))
	for k, c := range keyCounts {
		keys = append(keys, map[string]any{"api_key_prefix": k, "requests": c.Requests, "tokens": c.Tokens})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i]["requests"].(int64) > keys[j]["requests"].(int64) })

	trend := make([]map[string]any, 0, len(trendMap))
	for _, t := range trendMap {
		trend = append(trend, map[string]any{"date": t.Date, "requests": t.Requests, "tokens": t.Tokens})
	}
	sort.Slice(trend, func(i, j int) bool { return trend[i]["date"].(string) < trend[j]["date"].(string) })

	return map[string]any{
		"summary": map[string]any{
			"requests":         requests,
			"tokens":           in + out + cache,
			"input":            in,
			"output":           out,
			"cache":            cache,
			"avg_ms":           avgMs,
			"today_requests":   todayReq,
			"today_tokens":     todayTok,
			"last24h_requests": h24Req,
			"last24h_tokens":   h24Tok,
		},
		"models":    model,
		"endpoints": ep,
		"keys":      keys,
		"trend":     trend,
	}
}

func (s *usageLog) logs(limit, offset int) map[string]any {
	s.mu.Lock()
	recs := append([]UsageRecord(nil), s.records...)
	s.mu.Unlock()

	total := len(recs)
	if offset > total {
		offset = total
	}
	start := total - offset - limit
	if start < 0 {
		start = 0
	}
	end := total - offset
	if end < 0 {
		end = 0
	}
	if start >= end {
		return map[string]any{"logs": []UsageRecord{}, "total": total}
	}
	out := make([]UsageRecord, 0, end-start)
	for i := start; i < end; i++ {
		out = append(out, recs[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return map[string]any{"logs": out, "total": total}
}

type usageCountStat struct {
	Requests int64
	Tokens   int64
}

type usageTrendPoint struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}
