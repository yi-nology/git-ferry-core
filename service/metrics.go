package service

import (
	"github.com/oekit/metrics"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/yi-nology/git-ferry-core/model"
)

// ===== oekit/metrics 指标 =====
//
// 宿主用 metrics.Init + Prometheus handler 暴露;core 只注册指标并埋点,
// 不绑定任何 HTTP 框架。指标基数受控:标签只放 status / trigger 这类有界枚举。
var (
	syncRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gitferry_sync_runs_total",
		Help: "完成的同步运行数,按状态与触发源。",
	}, []string{"status", "trigger"})

	syncRunDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gitferry_sync_run_duration_seconds",
		Help:    "同步运行耗时(秒)。",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 14400},
	}, []string{"status"})

	executionQueuePending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gitferry_execution_queue_pending",
		Help: "持久化执行队列中待认领作业数。",
	})

	// provider 缓存(SDK runtime.Manager,64 条 LRU/30min TTL)。
	// 累计计数器用 Gauge 暴露当前快照:值只增不减,抓取方按 counter 用法读即可。
	providerCacheHits = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gitferry_provider_cache_hits_total",
		Help: "provider 缓存命中累计数。",
	})
	providerCacheMisses = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gitferry_provider_cache_misses_total",
		Help: "provider 缓存未命中累计数。",
	})
	providerCacheEvictions = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gitferry_provider_cache_evictions_total",
		Help: "provider 缓存淘汰累计数。",
	})
	providerCacheSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gitferry_provider_cache_size",
		Help: "provider 缓存当前条目数。",
	})
)

func init() {
	metrics.RegisterReuse(metrics.Registry, syncRunsTotal)
	metrics.RegisterReuse(metrics.Registry, syncRunDuration)
	metrics.RegisterReuse(metrics.Registry, executionQueuePending)
	metrics.RegisterReuse(metrics.Registry, providerCacheHits)
	metrics.RegisterReuse(metrics.Registry, providerCacheMisses)
	metrics.RegisterReuse(metrics.Registry, providerCacheEvictions)
	metrics.RegisterReuse(metrics.Registry, providerCacheSize)
}

// observeRunMetrics 记录一次完成的同步运行。
func observeRunMetrics(run *model.SyncRun) {
	if run == nil {
		return
	}
	syncRunsTotal.WithLabelValues(run.Status, run.TriggerSource).Inc()
	if run.EndTime != nil && !run.StartTime.IsZero() {
		syncRunDuration.WithLabelValues(run.Status).
			Observe(run.EndTime.Sub(run.StartTime).Seconds())
	}
}
