//go:build integration
// +build integration

package integration_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	vai "github.com/vango-go/vai-lite/sdk"
)

var providerThrottleState = struct {
	mu   sync.Mutex
	last map[string]time.Time
}{
	last: make(map[string]time.Time),
}

type providerErrorClass struct {
	Retryable bool
	Capacity  bool
	Reason    string
}

func runWithProviderRetry[T any](
	t *testing.T,
	provider providerConfig,
	timeout time.Duration,
	operation string,
	fn func(ctx context.Context) (T, error),
) (T, error) {
	t.Helper()

	var zero T
	attempts := providerRetryAttempts(provider.Name)
	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		enforceProviderThrottle(provider.Name)
		ctx := testContext(t, timeout)

		result, err := fn(ctx)
		if err == nil {
			return result, nil
		}

		lastErr = err
		class := classifyProviderError(err)
		logProviderDiagnostic(t, provider, operation, attempt, class, err)

		if !class.Retryable || attempt == attempts {
			if provider.Name == "gem-vert" && class.Capacity {
				t.Skipf(
					"Skipping %s/%s after %d attempts due provider capacity (%s): %v",
					provider.Name,
					operation,
					attempt,
					class.Reason,
					err,
				)
			}
			return zero, err
		}

		backoff := retryBackoff(provider.Name, attempt, class.Capacity)
		t.Logf(
			"Retrying %s/%s in %s after transient error (%s): %v",
			provider.Name,
			operation,
			backoff,
			class.Reason,
			err,
		)
		time.Sleep(backoff)
	}

	return zero, lastErr
}

func enforceProviderThrottle(provider string) {
	minGap := providerMinGap(provider)
	if minGap <= 0 {
		return
	}

	for {
		providerThrottleState.mu.Lock()
		last := providerThrottleState.last[provider]
		now := time.Now()
		wait := minGap - now.Sub(last)
		if wait <= 0 {
			providerThrottleState.last[provider] = now
			providerThrottleState.mu.Unlock()
			return
		}
		providerThrottleState.mu.Unlock()
		time.Sleep(wait)
	}
}

func providerMinGap(provider string) time.Duration {
	switch provider {
	case "gem-vert":
		return time.Duration(envInt("VAI_INTEGRATION_GEM_VERT_MIN_GAP_MS", 2500)) * time.Millisecond
	case "gem-dev":
		return time.Duration(envInt("VAI_INTEGRATION_GEM_DEV_MIN_GAP_MS", 750)) * time.Millisecond
	default:
		return time.Duration(envInt("VAI_INTEGRATION_MIN_GAP_MS", 0)) * time.Millisecond
	}
}

func providerRetryAttempts(provider string) int {
	global := envInt("VAI_INTEGRATION_RETRY_ATTEMPTS", 1)
	if global < 1 {
		global = 1
	}

	switch provider {
	case "gem-vert":
		n := envInt("VAI_INTEGRATION_GEM_VERT_RETRY_ATTEMPTS", 4)
		if n > global {
			return n
		}
	case "gem-dev":
		n := envInt("VAI_INTEGRATION_GEM_DEV_RETRY_ATTEMPTS", 3)
		if n > global {
			return n
		}
	}
	return global
}

func retryBackoff(provider string, attempt int, capacity bool) time.Duration {
	base := time.Duration(envInt("VAI_INTEGRATION_RETRY_BASE_MS", 750)) * time.Millisecond
	maxWait := time.Duration(envInt("VAI_INTEGRATION_RETRY_MAX_MS", 10000)) * time.Millisecond

	wait := base
	for i := 1; i < attempt; i++ {
		wait *= 2
		if wait >= maxWait {
			wait = maxWait
			break
		}
	}

	if capacity && provider == "gem-vert" {
		wait *= 2
		if wait > maxWait {
			wait = maxWait
		}
	}

	return wait
}

func classifyProviderError(err error) providerErrorClass {
	if err == nil {
		return providerErrorClass{}
	}
	if errors.Is(err, context.Canceled) {
		return providerErrorClass{Reason: "context canceled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return providerErrorClass{Reason: "deadline exceeded"}
	}

	lower := strings.ToLower(err.Error())

	capacityMarkers := []string{
		"resource_exhausted",
		"rate_limit",
		"rate limit",
		"quota",
		"too many requests",
		"429",
	}
	for _, marker := range capacityMarkers {
		if strings.Contains(lower, marker) {
			return providerErrorClass{
				Retryable: true,
				Capacity:  true,
				Reason:    marker,
			}
		}
	}

	transientMarkers := []string{
		"unavailable",
		"overloaded",
		"temporarily unavailable",
		"internal",
		"503",
		"504",
		"502",
		"connection reset",
		"timeout",
		"try again",
	}
	for _, marker := range transientMarkers {
		if strings.Contains(lower, marker) {
			return providerErrorClass{
				Retryable: true,
				Reason:    marker,
			}
		}
	}

	return providerErrorClass{}
}

func isModelToolRefusal(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "tool_use_failed") || strings.Contains(lower, "did not call a tool")
}

func logProviderDiagnostic(
	t *testing.T,
	provider providerConfig,
	operation string,
	attempt int,
	class providerErrorClass,
	err error,
) {
	t.Helper()
	if !integrationDiagnosticsEnabled() {
		return
	}

	t.Logf(
		"diagnostic provider=%s operation=%s attempt=%d retryable=%t capacity=%t reason=%q err=%q",
		provider.Name,
		operation,
		attempt,
		class.Retryable,
		class.Capacity,
		class.Reason,
		err.Error(),
	)
}

func integrationDiagnosticsEnabled() bool {
	return envBool("VAI_INTEGRATION_DIAGNOSTICS", false)
}

func envInt(name string, defaultValue int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return defaultValue
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return defaultValue
	}
	return v
}

func envBool(name string, defaultValue bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if raw == "" {
		return defaultValue
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return defaultValue
	}
}

func toolResultsContainText(result *vai.RunResult, needle string) bool {
	if result == nil {
		return false
	}
	for _, step := range result.Steps {
		for _, tr := range step.ToolResults {
			for _, block := range tr.Content {
				if tb, ok := block.(vai.TextBlock); ok {
					if strings.Contains(strings.ToLower(tb.Text), strings.ToLower(needle)) {
						return true
					}
				}
			}
		}
	}
	return false
}
