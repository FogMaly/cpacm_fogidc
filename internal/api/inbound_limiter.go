package api

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type tokenBucket struct {
	tokens float64
	last   time.Time
}

type inboundLimiter struct {
	perKeyQPS         float64
	globalConcurrency int

	mu         sync.Mutex
	keyBuckets map[string]*tokenBucket
	inFlight   int
}

type inboundLimiterStatus struct {
	Enabled           bool    `json:"enabled"`
	PerKeyQPS         float64 `json:"per_key_qps"`
	GlobalConcurrency int     `json:"global_concurrency"`
	InFlight          int     `json:"in_flight"`
	TrackedKeys       int     `json:"tracked_keys"`
}

func newInboundLimiter(cfg *config.Config) *inboundLimiter {
	if cfg == nil {
		return nil
	}
	limits := cfg.InboundRateLimit
	if limits.PerKeyQPS <= 0 && limits.GlobalConcurrency <= 0 {
		return nil
	}
	return &inboundLimiter{
		perKeyQPS:         limits.PerKeyQPS,
		globalConcurrency: limits.GlobalConcurrency,
		keyBuckets:        make(map[string]*tokenBucket),
	}
}

func (l *inboundLimiter) AcquireGlobal() (func(), bool) {
	if l == nil || l.globalConcurrency <= 0 {
		return nil, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight >= l.globalConcurrency {
		return nil, false
	}
	l.inFlight++
	return func() {
		l.mu.Lock()
		if l.inFlight > 0 {
			l.inFlight--
		}
		l.mu.Unlock()
	}, true
}

func (l *inboundLimiter) AllowKey(key string) bool {
	if l == nil || l.perKeyQPS <= 0 {
		return true
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket := l.keyBuckets[key]
	if bucket == nil {
		bucket = &tokenBucket{tokens: l.perKeyQPS, last: now}
		l.keyBuckets[key] = bucket
	}
	elapsed := now.Sub(bucket.last).Seconds()
	bucket.tokens += elapsed * l.perKeyQPS
	if bucket.tokens > l.perKeyQPS {
		bucket.tokens = l.perKeyQPS
	}
	bucket.last = now
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens -= 1
	return true
}

func (l *inboundLimiter) Status() inboundLimiterStatus {
	if l == nil {
		return inboundLimiterStatus{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return inboundLimiterStatus{
		Enabled:           l.perKeyQPS > 0 || l.globalConcurrency > 0,
		PerKeyQPS:         l.perKeyQPS,
		GlobalConcurrency: l.globalConcurrency,
		InFlight:          l.inFlight,
		TrackedKeys:       len(l.keyBuckets),
	}
}

func globalConcurrencyMiddleware(limiter *inboundLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limiter == nil {
			c.Next()
			return
		}
		release, ok := limiter.AcquireGlobal()
		if !ok {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "global concurrency limit exceeded"})
			return
		}
		if release == nil {
			c.Next()
			return
		}
		defer release()
		c.Next()
	}
}
