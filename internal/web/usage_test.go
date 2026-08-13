package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestUsageLog 构造一个测试用 usageLog。persist 用 no-op flush：避免 markDirty
// 把 store 注册进全局 persistLoop 后台 goroutine 后，该 goroutine 调用 nil flush 造成
// panic；同时隔离磁盘写入，仅由本测试直接调用 s.flush() 触发。
func newTestUsageLog(path string) *usageLog {
	return &usageLog{Path: path, persist: &persistStore{flush: func() error { return nil }}}
}

// T1 配置解析：M365_USAGE_RETENTION_HOURS 的解析语义（G1）。
func TestOpenUsageLogRetentionParsing(t *testing.T) {
	cases := []struct {
		name string
		env  string // 未设置用空串表示
		want time.Duration
	}{
		{"unset", "", 2 * time.Hour},
		{"one", "1", 1 * time.Hour},
		{"zero", "0", 0},
		{"negative", "-3", 0},
		{"noninteger", "abc", 2 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 隔离到临时文件，避免读入真实数据目录
			t.Setenv("M365_DATA_DIR", t.TempDir())
			t.Setenv("M365_USAGE_LOG", filepath.Join(t.TempDir(), "usage.jsonl"))
			if c.env == "" {
				t.Setenv("M365_USAGE_RETENTION_HOURS", "unset-marker")
				os.Unsetenv("M365_USAGE_RETENTION_HOURS")
			} else {
				t.Setenv("M365_USAGE_RETENTION_HOURS", c.env)
			}
			s := openUsageLog()
			if s.retention != c.want {
				t.Fatalf("retention = %v, want %v", s.retention, c.want)
			}
		})
	}
}

// T2 时间 + 计数组合裁剪（G2）。
func TestUsageTrimTimeAndCount(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour)

	// retention>0：丢弃 cutoff 之前的记录，窗口内保留
	t.Run("time_trim", func(t *testing.T) {
		s := &usageLog{retention: 1 * time.Hour, persist: &persistStore{}}
		s.records = []UsageRecord{
			{Time: old, APIKeyPrefix: "old"},
			{Time: now, APIKeyPrefix: "new1"},
			{Time: now, APIKeyPrefix: "new2"},
		}
		s.trim()
		if len(s.records) != 2 {
			t.Fatalf("after time trim len = %d, want 2", len(s.records))
		}
		for _, r := range s.records {
			if r.APIKeyPrefix == "old" {
				t.Fatal("old record should have been trimmed by time")
			}
		}
	})

	// retention==0：时间裁剪不发生，仅计数裁剪；数量上限始终兜底
	t.Run("retention_zero_keeps_time", func(t *testing.T) {
		s := &usageLog{retention: 0, persist: &persistStore{}}
		s.records = []UsageRecord{
			{Time: old, APIKeyPrefix: "old"},
			{Time: now, APIKeyPrefix: "new1"},
			{Time: now, APIKeyPrefix: "new2"},
		}
		s.trim()
		if len(s.records) != 3 {
			t.Fatalf("retention==0 must keep all 3 by time, got %d", len(s.records))
		}
	})

	// 计数上限始终生效：retention>0 且记录数超过 maxUsageRecords 时截断到上限
	t.Run("count_ceiling", func(t *testing.T) {
		s := &usageLog{retention: 1 * time.Hour, persist: &persistStore{}}
		recs := make([]UsageRecord, 0, maxUsageRecords+5)
		for i := 0; i < maxUsageRecords+5; i++ {
			recs = append(recs, UsageRecord{Time: now, APIKeyPrefix: "k"})
		}
		s.records = recs
		s.trim()
		if len(s.records) != maxUsageRecords {
			t.Fatalf("count ceiling len = %d, want %d", len(s.records), maxUsageRecords)
		}
	})

	// retention>0 且既有超窗口老记录又有超计数时，先时间后计数
	t.Run("time_then_count", func(t *testing.T) {
		s := &usageLog{retention: 1 * time.Hour, persist: &persistStore{}}
		recs := make([]UsageRecord, 0, maxUsageRecords+10)
		recs = append(recs, UsageRecord{Time: old, APIKeyPrefix: "old"})
		for i := 0; i < maxUsageRecords+9; i++ {
			recs = append(recs, UsageRecord{Time: now, APIKeyPrefix: "k"})
		}
		s.records = recs
		s.trim()
		if len(s.records) > maxUsageRecords {
			t.Fatalf("len = %d, must be <= %d", len(s.records), maxUsageRecords)
		}
		for _, r := range s.records {
			if r.APIKeyPrefix == "old" {
				t.Fatal("old record should have been trimmed before count ceiling")
			}
		}
	})
}

// T3 联动清理 purgePrefix（G5 单元层）。
func TestUsagePurgePrefix(t *testing.T) {
	s := newTestUsageLog("")
	s.records = []UsageRecord{
		{APIKeyPrefix: "aaa..."},
		{APIKeyPrefix: "bbb..."},
		{APIKeyPrefix: "aaa..."},
	}
	s.pending = []UsageRecord{
		{APIKeyPrefix: "aaa..."},
		{APIKeyPrefix: "ccc..."},
	}
	s.purgePrefix("aaa...")
	if s.persist == nil || !s.persist.dirty {
		t.Fatal("expected markDirty after purgePrefix")
	}
	for _, r := range s.records {
		if r.APIKeyPrefix == "aaa..." {
			t.Fatal("records still contain purged prefix")
		}
	}
	for _, r := range s.pending {
		if r.APIKeyPrefix == "aaa..." {
			t.Fatal("pending still contain purged prefix")
		}
	}
	if !hasPrefix(s.records, "bbb...") {
		t.Fatal("bbb... should be retained in records")
	}
	if !hasPrefix(s.pending, "ccc...") {
		t.Fatal("ccc... should be retained in pending")
	}

	// 空 prefix 早退、无副作用
	before := len(s.records)
	s.purgePrefix("")
	if len(s.records) != before {
		t.Fatal("empty prefix must be a no-op")
	}

	// 不存在的 prefix 也无副作用
	s.purgePrefix("zzz...")
	if len(s.records) != before {
		t.Fatal("unknown prefix must be a no-op")
	}
}

func hasPrefix(recs []UsageRecord, p string) bool {
	for _, r := range recs {
		if r.APIKeyPrefix == p {
			return true
		}
	}
	return false
}

// T4 整文件重写：rewrite 而非 append（G3）。
func TestUsageFlushRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	s := newTestUsageLog(path)
	s.records = []UsageRecord{
		{Time: time.Now(), APIKeyPrefix: "x"},
		{Time: time.Now(), APIKeyPrefix: "y"},
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if n := countLines(t, path); n != 2 {
		t.Fatalf("after first flush lines = %d, want 2", n)
	}

	// 再次 flush 不应叠加旧行
	s.records = append(s.records, UsageRecord{Time: time.Now(), APIKeyPrefix: "z"})
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if n := countLines(t, path); n != 3 {
		t.Fatalf("after second flush lines = %d, want 3 (no append duplication)", n)
	}
}

// T5 写入/同步/重命名失败路径（G3 健壮性、Windows 失败恢复语义）。
func TestUsageFlushFailurePaths(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "missing-parent", "usage.jsonl")
	s := newTestUsageLog(badPath)
	s.records = []UsageRecord{{Time: time.Now(), APIKeyPrefix: "x"}}
	if err := s.flush(); err == nil {
		t.Fatal("expected error when writing to missing parent dir")
	}
	if s.persist == nil || !s.persist.dirty {
		t.Fatal("expected markDirty after failed flush (retry on next tick)")
	}
	if len(s.records) != 1 {
		t.Fatalf("in-memory records must not be lost on flush failure, got %d", len(s.records))
	}

	// 改写到有效路径后应成功且内容完整
	goodPath := filepath.Join(t.TempDir(), "usage2.jsonl")
	s.Path = goodPath
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if n := countLines(t, goodPath); n != 1 {
		t.Fatalf("retry flush lines = %d, want 1", n)
	}
}

// T6 旧文件迁移与重启：启动加载旧大文件后首轮重写压缩（G4）。
func TestUsageLoadMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	var b []byte
	old := time.Now().Add(-3 * time.Hour)
	const total = 1000
	for i := 0; i < total; i++ {
		rec := UsageRecord{Time: old, APIKeyPrefix: "k"}
		j, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, j...)
		b = append(b, '\n')
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("M365_USAGE_RETENTION_HOURS", "2")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_USAGE_LOG", path)
	s := openUsageLog()

	if len(s.records) >= total {
		t.Fatalf("expected time trimming, got %d records (>= %d)", len(s.records), total)
	}
	if s.persist == nil || !s.persist.dirty {
		t.Fatal("expected markDirty for migration rewrite")
	}
	// 首轮重写落盘后，磁盘行数 == 裁剪后内存记录数
	s.persist.flushPending()
	if n := countLines(t, path); n != len(s.records) {
		t.Fatalf("after migration rewrite file lines = %d, want %d (== trimmed records)", n, len(s.records))
	}
}

// T7 DELETE 联动端到端：删除 key 后用量该前缀被 purge（G5 单元层端到端）。
func TestDeleteKeyPurgesUsage(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	rec, _, err := store.create("temp")
	if err != nil {
		t.Fatal(err)
	}
	// 用量侧前缀写法和运行时一致：extractAPIKey(raw) == raw[:8]+"..."
	usagePrefix := rec.Prefix
	if len(usagePrefix) > 8 {
		usagePrefix = usagePrefix[:8] + "..."
	}
	usage := newTestUsageLog("")
	usage.records = []UsageRecord{
		{APIKeyPrefix: usagePrefix},
		{APIKeyPrefix: "other..."},
	}

	prefix, deleted, e := store.delete(rec.ID)
	if e != nil {
		t.Fatal(e)
	}
	if !deleted {
		t.Fatal("expected delete ok")
	}
	if prefix != rec.Prefix {
		t.Fatalf("returned prefix = %q, want %q", prefix, rec.Prefix)
	}

	up := prefix
	if len(up) > 8 {
		up = up[:8] + "..."
	}
	usage.purgePrefix(up)

	if hasPrefix(usage.records, usagePrefix) {
		t.Fatal("usage records should no longer contain deleted key prefix")
	}
	if hasPrefix(usage.records, "other...") == false {
		t.Fatal("other prefix must be retained")
	}
	// keys 列表不应再含该 id
	for _, k := range store.Keys {
		if k.ID == rec.ID {
			t.Fatal("key still present in store after delete")
		}
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) == 0 {
		return 0
	}
	return strings.Count(string(content), "\n")
}
