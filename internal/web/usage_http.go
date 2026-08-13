package web

import (
	"net/http"
	"strconv"
)

func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 本项目为临时/自用网关，无保留长周期日志的必要；days 仅决定从“已保留”记录中
	// 按时间过滤展示的跨度，真实磁盘保留窗口由 M365_USAGE_RETENTION_HOURS 控制。
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	jsonOut(w, map[string]any{
		"days":  days,
		"stats": s.usage.snapshot(days),
	})
}

func (s *Server) adminUsageLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 50
	offset := 0
	q := r.URL.Query()
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	jsonOut(w, s.usage.logs(limit, offset))
}
