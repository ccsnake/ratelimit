package ratelimit

import (
	"crypto/sha1"
	"fmt"
	"github.com/go-redis/redis"
	"sync"
	"time"
)

// time_unit is the second of unit
// 最小精度是秒
const SCRIPT = `
local current_timestamp = redis.call("TIME")
local key_prefix = KEYS[1]
local duration_secs = tonumber(ARGV[1])
local throughput = tonumber(ARGV[2])
local batch_size = tonumber(ARGV[3])
local key = key_prefix .. ":" .. tostring(math.ceil(tonumber(current_timestamp[1])/duration_secs))
local n = redis.call("GET", key)

if n == false then
    n = 0
else
    n = tonumber(n)
end

if n >= throughput then
    return 0
end

local increment = math.min(throughput - n, batch_size)
redis.replicate_commands();
redis.call("INCRBY", key, increment)
redis.call("EXPIRE", key, duration_secs * 3)
return increment
`

var scriptSHA1 string
var once sync.Once

type bucket struct {
	keyPrefix string
	N         int64
}

type RedisRateLimiter struct {
	redisClient *redis.Client
	scriptSHA1  string
	// config
	durationSecs int
	throughput   int
	batchSize    int

	// fixme replace with lru
	sync.Mutex
	keyPrefix string
	buckets   map[string]*bucket
}

// duration 精度最小到秒
//

func NewRedisRateLimiter(client *redis.Client, keyPrefix string,
	duration time.Duration, throughput int, batchSize int) (*RedisRateLimiter) {

	durationSecs := duration / time.Second
	if durationSecs < 1 {
		durationSecs = 1
	}

	r := &RedisRateLimiter{
		redisClient: client,
		keyPrefix:   keyPrefix,
		// scriptSHA1:   fmt.Sprintf("%x", sha1.Sum([]byte(SCRIPT))),
		durationSecs: int(durationSecs),
		throughput:   throughput,
		batchSize:    batchSize,
		buckets:      make(map[string]*bucket),
	}

	// if !r.redisClient.ScriptExists(r.scriptSHA1).Val()[0] {
	// 	r.scriptSHA1 = r.redisClient.ScriptLoad(SCRIPT).Val()
	// }
	return r
}

func (r *RedisRateLimiter) loadScript() error {
	sha1 := fmt.Sprintf("%x", sha1.Sum([]byte(SCRIPT)))

	exist, err := r.redisClient.ScriptExists(sha1).Result()
	if err != nil {
		return err
	}

	// exist
	if exist[0] {
		r.scriptSHA1 = sha1
		return nil
	}
	r.scriptSHA1, err = r.redisClient.ScriptLoad(SCRIPT).Result()
	return err
}

func (r *RedisRateLimiter) Take(token string, amount int) (bool, error) {
	r.Lock()
	defer r.Unlock()

	if r.scriptSHA1 == "" {
		if err := r.loadScript(); err != nil {
			return false, err
		}
	}

	b, exist := r.buckets[token]
	if exist && b.N >= int64(amount) {
		b.N -= int64(amount)
		return true, nil
	}

	val, err := r.redisClient.EvalSha(r.scriptSHA1, []string{token}, r.durationSecs, r.throughput, r.batchSize, ).Result()
	if err != nil {
		return false, err
	}

	count := val.(int64)
	if count <= 0 {
		return false, nil
	}

	if exist {
		b.N = b.N + count
	} else {
		b = &bucket{keyPrefix: r.keyPrefix + ":" + token, N: count}
		r.buckets[token] = b
	}

	if b.N >= int64(amount) {
		b.N -= int64(amount)
		return true, nil
	}
	return false, nil
}
