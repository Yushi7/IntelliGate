package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultRedisAddr                   = "localhost:6379"
	defaultRedisNamespace              = "intelligate:ratelimit:v1"
	defaultRedisOperationTimeout       = 2 * time.Second
	defaultRedisStartupTimeout         = 2 * time.Second
	redisTokenScale              int64 = 1_000_000
	maxRedisFixedUnits           int64 = 1_000_000_000
)

type RedisOptions struct {
	Addr             string
	Password         string
	DB               int
	Namespace        string
	OperationTimeout time.Duration
	StartupTimeout   time.Duration
}

func DefaultRedisOptions() RedisOptions {
	return RedisOptions{
		Addr:             defaultRedisAddr,
		OperationTimeout: defaultRedisOperationTimeout,
		StartupTimeout:   defaultRedisStartupTimeout,
	}
}

type RedisStore struct {
	client           *redis.Client
	namespace        string
	operationTimeout time.Duration
	rateUnits        int64
	burstUnits       int64
	idleTimeoutMS    int64
}

func NewRedisStore(config Config, options RedisOptions) (*RedisStore, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	if err := validateRedisOptions(options); err != nil {
		return nil, err
	}

	client := redis.NewClient(&redis.Options{
		Addr:         options.Addr,
		Password:     options.Password,
		DB:           options.DB,
		DialTimeout:  options.OperationTimeout,
		ReadTimeout:  options.OperationTimeout,
		WriteTimeout: options.OperationTimeout,
		MaxRetries:   -1,
	})
	store, err := newRedisStore(client, normalized, options)
	if err != nil {
		_ = client.Close()
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), options.StartupTimeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("Redis rate-limit startup connectivity check failed: %w", err)
	}
	return store, nil
}

func newRedisStore(client *redis.Client, config Config, options RedisOptions) (*RedisStore, error) {
	if client == nil {
		return nil, fmt.Errorf("Redis client must not be nil")
	}
	rateUnits, err := redisFixedUnits(config.Rate)
	if err != nil {
		return nil, fmt.Errorf("Redis rate-limit rate: %w", err)
	}
	burstUnits, err := redisFixedUnits(config.Burst)
	if err != nil {
		return nil, fmt.Errorf("Redis rate-limit burst: %w", err)
	}
	if config.IdleTimeout.Milliseconds() < 1 {
		return nil, fmt.Errorf("Redis rate-limit idle timeout must be at least one millisecond")
	}
	if err := validateRedisOptions(options); err != nil {
		return nil, err
	}
	namespace := options.Namespace
	if namespace == "" {
		namespace = defaultRedisNamespace
	} else {
		namespace = defaultRedisNamespace + ":" + namespace
	}
	return &RedisStore{
		client:           client,
		namespace:        namespace,
		operationTimeout: options.OperationTimeout,
		rateUnits:        rateUnits,
		burstUnits:       burstUnits,
		idleTimeoutMS:    config.IdleTimeout.Milliseconds(),
	}, nil
}

func validateRedisOptions(options RedisOptions) error {
	if err := ValidateRedisAddress(options.Addr); err != nil {
		return err
	}
	if options.DB < 0 {
		return fmt.Errorf("Redis database number must not be negative")
	}
	if options.OperationTimeout <= 0 {
		return fmt.Errorf("Redis operation timeout must be greater than zero")
	}
	if options.StartupTimeout <= 0 {
		return fmt.Errorf("Redis startup timeout must be greater than zero")
	}
	if options.Namespace != "" {
		for _, character := range options.Namespace {
			if !(character >= 'a' && character <= 'z' ||
				character >= 'A' && character <= 'Z' ||
				character >= '0' && character <= '9' ||
				character == '-' || character == '_' || character == '.') {
				return fmt.Errorf("Redis namespace may contain only letters, digits, dot, underscore and hyphen")
			}
		}
	}
	return nil
}

func ValidateRedisAddress(address string) error {
	if strings.TrimSpace(address) != address || address == "" {
		return fmt.Errorf("Redis address must be a host:port value")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return fmt.Errorf("Redis address must be a valid host:port value")
	}
	for _, character := range port {
		if character < '0' || character > '9' {
			return fmt.Errorf("Redis address must include a numeric port")
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("Redis address must include a port between 1 and 65535")
	}
	if !validRedisHost(host) {
		return fmt.Errorf("Redis address contains an invalid host")
	}
	return nil
}

func validRedisHost(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 ||
			!isRedisHostnameCharacter(label[0]) || !isRedisHostnameCharacter(label[len(label)-1]) {
			return false
		}
		for i := 1; i < len(label)-1; i++ {
			character := label[i]
			if !isRedisHostnameCharacter(character) && character != '-' {
				return false
			}
		}
	}
	return true
}

func isRedisHostnameCharacter(character byte) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9'
}

func redisFixedUnits(value float64) (int64, error) {
	scaled := math.Round(value * float64(redisTokenScale))
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled < 1 || scaled > float64(maxRedisFixedUnits) {
		return 0, fmt.Errorf("must be representable between 0.000001 and 1000 tokens")
	}
	return int64(scaled), nil
}

func (s *RedisStore) Consume(ctx context.Context, clientIP string) (bool, int64, error) {
	address, err := netip.ParseAddr(clientIP)
	if err != nil {
		return false, 0, fmt.Errorf("invalid canonical client IP: %w", err)
	}
	clientIP = address.Unmap().String()
	operationContext, cancel := context.WithTimeout(ctx, s.operationTimeout)
	defer cancel()

	values, err := redisBucketScript.Run(
		operationContext,
		s.client,
		[]string{s.key(clientIP)},
		redisTokenScale,
		s.rateUnits,
		s.burstUnits,
		s.idleTimeoutMS,
	).Slice()
	if err != nil {
		return false, 0, fmt.Errorf("Redis token-bucket operation failed: %w", err)
	}
	if len(values) != 2 {
		return false, 0, fmt.Errorf("Redis token-bucket operation returned an invalid result")
	}
	allowed, ok := values[0].(int64)
	if !ok {
		return false, 0, fmt.Errorf("Redis token-bucket operation returned an invalid decision")
	}
	retryAfter, ok := values[1].(int64)
	if !ok {
		return false, 0, fmt.Errorf("Redis token-bucket operation returned an invalid retry interval")
	}
	return allowed == 1, retryAfter, nil
}

func (s *RedisStore) key(clientIP string) string {
	digest := sha256.Sum256([]byte(clientIP))
	return s.namespace + ":" + hex.EncodeToString(digest[:])
}

func (s *RedisStore) Close() error {
	return s.client.Close()
}

var redisBucketScript = redis.NewScript(`
local scale = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])
local ttl_ms = tonumber(ARGV[4])

local server_time = redis.call('TIME')
local now_us = tonumber(server_time[1]) * 1000000 + tonumber(server_time[2])
local state = redis.call('HMGET', KEYS[1], 'tokens', 'last_us', 'remainder')
local tokens
local last_us
local remainder

if not state[1] and not state[2] and not state[3] then
    tokens = burst
    last_us = now_us
    remainder = 0
else
    tokens = tonumber(state[1])
    last_us = tonumber(state[2])
    remainder = tonumber(state[3])
    if not tokens or not last_us or not remainder or
        tokens < 0 or tokens > burst or remainder < 0 or remainder >= 1000000 then
        return redis.error_reply('invalid token bucket state')
    end
end

if now_us < last_us then
    now_us = last_us
end
local elapsed_us = now_us - last_us
local missing = burst - tokens
if missing > 0 and elapsed_us > 0 then
    local time_to_full_us = math.ceil(missing * 1000000 / rate)
    if elapsed_us >= time_to_full_us then
        tokens = burst
        remainder = 0
    else
        local numerator = elapsed_us * rate + remainder
        local refill = math.floor(numerator / 1000000)
        remainder = numerator % 1000000
        tokens = math.min(burst, tokens + refill)
        if tokens >= burst then
            remainder = 0
        end
    end
end

local allowed = 0
local retry_after = 0
if tokens >= scale then
    tokens = tokens - scale
    allowed = 1
else
    retry_after = math.max(1, math.ceil((scale - tokens) / rate))
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'last_us', now_us, 'remainder', remainder)
redis.call('PEXPIRE', KEYS[1], ttl_ms)
return {allowed, retry_after}
`)
