package isureal

import (
	"context"
	"log"
	"time"

	"github.com/jmoiron/sqlx"
)

const imageURLWarmupBudget = 30 * time.Second
const imageURLWarmupStageBudget = 12 * time.Second

var imageURLWarmupQueries = []string{
	`SELECT secret_key_hash, profile_image_id AS image_id FROM users
 WHERE status='active' AND profile_image_id IS NOT NULL ORDER BY id LIMIT 8192`,
	`SELECT u.secret_key_hash, p.image_id FROM posts p JOIN users u ON u.id=p.author_id
 WHERE p.deleted_at IS NULL AND u.status='active'
 ORDER BY p.posted_at DESC, p.id DESC LIMIT 24576`,
	`SELECT u.secret_key_hash, r.image_id FROM realmojis r JOIN users u ON u.id=r.reactor_id
 WHERE r.deleted_at IS NULL AND r.replaced_at IS NULL AND u.status='active'
 ORDER BY r.created_at DESC, r.id DESC LIMIT 24576`,
}

// Optional acceleration only: failure or cancellation leaves ordinary cache-miss
// signing available. No work continues after initialize returns.
func warmImageURLs(ctx context.Context, db *sqlx.DB, cache *imageURLCache) int {
	started := time.Now()
	// Leave room for initialize's response before its existing deadline.
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 2*time.Second {
		return 0
	}
	deadline := started.Add(imageURLWarmupBudget)
	if parent, ok := ctx.Deadline(); ok && parent.Add(-2*time.Second).Before(deadline) {
		deadline = parent.Add(-2 * time.Second)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	total := 0
	for i, query := range imageURLWarmupQueries {
		if ctx.Err() != nil {
			break
		}
		// Give each class time, even when a preceding class cannot finish.
		stageCtx, stop := context.WithTimeout(ctx, imageURLWarmupStageBudget)
		count, err := warmImageURLQuery(stageCtx, db, cache, query)
		stop()
		total += count
		if err != nil {
			log.Printf("initialize image_url_warmup stage=%d count=%d error=%v", i, count, err)
		}
	}
	log.Printf("initialize image_url_warmup count=%d duration=%s", total, time.Since(started))
	return total
}

func warmImageURLQuery(ctx context.Context, db *sqlx.DB, cache *imageURLCache, query string) (int, error) {
	rows, err := db.QueryxContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		var hash, id string
		if err := rows.Scan(&hash, &id); err != nil {
			return count, err
		}
		key, err := imageSigningKey(hash)
		if err != nil {
			return count, err
		}
		cache.get(key, id, time.Now())
		count++
	}
	return count, rows.Err()
}
