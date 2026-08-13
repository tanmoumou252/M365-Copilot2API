package web

import (
	"sync"
	"time"
)

type CacheStats struct {
	mu sync.Mutex

	TotalRequests  int64         `json:"total_requests"`
	CacheHits      int64         `json:"cache_hits"`
	CacheMisses    int64         `json:"cache_misses"`
	TokensSent     int64         `json:"tokens_sent"`
	TokensSaved    int64         `json:"tokens_saved"`
	ActiveSessions int           `json:"active_sessions"`
	MaxSessionAge  time.Duration `json:"max_session_age"`
	HitRate        float64       `json:"hit_rate"`
	SavingsPercent float64       `json:"savings_percent"`

	// 按 API Key 统计
	KeyStats map[string]*KeyStat `json:"key_stats"`
}

// statsSnapshot 是无锁深拷贝快照，避免 GetStats 复制锁值。
type statsSnapshot struct {
	TotalRequests  int64         `json:"total_requests"`
	CacheHits      int64         `json:"cache_hits"`
	CacheMisses    int64         `json:"cache_misses"`
	TokensSent     int64         `json:"tokens_sent"`
	TokensSaved    int64         `json:"tokens_saved"`
	ActiveSessions int           `json:"active_sessions"`
	MaxSessionAge  time.Duration `json:"max_session_age"`
	HitRate        float64       `json:"hit_rate"`
	SavingsPercent float64       `json:"savings_percent"`
	KeyStats       map[string]*KeyStat `json:"key_stats"`
}

type KeyStat struct {
	APIKey        string    `json:"api_key"`
	TotalRequests int64     `json:"total_requests"`
	CacheHits     int64     `json:"cache_hits"`
	CacheMisses   int64     `json:"cache_misses"`
	TokensSent    int64     `json:"tokens_sent"`
	TokensSaved   int64     `json:"tokens_saved"`
	HitRate       float64   `json:"hit_rate"`
	LastUsed      time.Time `json:"last_used"`
}

var cacheStats = &CacheStats{
	KeyStats: make(map[string]*KeyStat),
}

func (s *CacheStats) RecordRequest(apiKey string, hit bool, tokensSent, tokensSaved int64, activeSessions int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.TotalRequests++
	s.TokensSent += tokensSent
	s.TokensSaved += tokensSaved
	s.ActiveSessions = activeSessions

	if hit {
		s.CacheHits++
	} else {
		s.CacheMisses++
	}

	if s.TotalRequests > 0 {
		s.HitRate = float64(s.CacheHits) / float64(s.TotalRequests) * 100
	}
	if s.TokensSent+s.TokensSaved > 0 {
		s.SavingsPercent = float64(s.TokensSaved) / float64(s.TokensSent+s.TokensSaved) * 100
	}

	// 按 API Key 统计
	ks, ok := s.KeyStats[apiKey]
	if !ok {
		ks = &KeyStat{APIKey: apiKey}
		s.KeyStats[apiKey] = ks
	}
	ks.TotalRequests++
	ks.TokensSent += tokensSent
	ks.TokensSaved += tokensSaved
	ks.LastUsed = time.Now()
	if hit {
		ks.CacheHits++
	} else {
		ks.CacheMisses++
	}
	if ks.TotalRequests > 0 {
		ks.HitRate = float64(ks.CacheHits) / float64(ks.TotalRequests) * 100
	}
}

func (s *CacheStats) GetStats() statsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make(map[string]*KeyStat, len(s.KeyStats))
	for k, v := range s.KeyStats {
		cp := *v
		keys[k] = &cp
	}
	return statsSnapshot{
		TotalRequests:  s.TotalRequests,
		CacheHits:      s.CacheHits,
		CacheMisses:    s.CacheMisses,
		TokensSent:     s.TokensSent,
		TokensSaved:    s.TokensSaved,
		ActiveSessions: s.ActiveSessions,
		MaxSessionAge:  s.MaxSessionAge,
		HitRate:        s.HitRate,
		SavingsPercent: s.SavingsPercent,
		KeyStats:       keys,
	}
}

func (s *CacheStats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TotalRequests = 0
	s.CacheHits = 0
	s.CacheMisses = 0
	s.TokensSent = 0
	s.TokensSaved = 0
	s.HitRate = 0
	s.SavingsPercent = 0
	s.KeyStats = make(map[string]*KeyStat)
}

func EstimateTokens(text string) int64 {
	return countTokens("", text)
}
