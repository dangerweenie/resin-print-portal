package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// WaitForSchema blocks until the migrations have created the core tables (or the
// timeout elapses). On a fresh Helm install the server can start before the
// post-install migrate Job finishes; this lets it wait instead of crash-looping.
func (s *Store) WaitForSchema(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var one int
		err := s.pool.QueryRow(ctx, `SELECT 1 FROM admins LIMIT 1`).Scan(&one)
		if err == nil || strings.Contains(err.Error(), "no rows") || err.Error() == "no rows in result set" {
			return nil
		}
		if !strings.Contains(err.Error(), "does not exist") {
			return fmt.Errorf("unexpected error waiting for schema: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("schema not ready after %s (has `portal migrate up` run?)", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
