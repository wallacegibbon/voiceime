package models

import (
	"context"
	"time"
)

// contextWithTimeout is the one place model discovery bounds its subprocesses.
// Kept in one file so the timeout policy is easy to find and change.
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
