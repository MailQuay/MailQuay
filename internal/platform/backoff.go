package platform

import (
	"math"
	"time"
)

// Backoff returns an exponential delay capped at max.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	mult := math.Pow(2, float64(attempt-1))
	delay := time.Duration(float64(base) * mult)
	if delay <= 0 || delay > max {
		return max
	}
	return delay
}
