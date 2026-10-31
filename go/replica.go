package isureal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// readDB permits both a pool and a request's consistent read-only snapshot.
type readDB interface {
	sqlx.QueryerContext
	GetContext(context.Context, any, string, ...any) error
	SelectContext(context.Context, any, string, ...any) error
}

type replicaRouter struct {
	primary, replica  *sqlx.DB
	api               *primaryAPIClient
	name, primaryHost string
	healthyAt         atomic.Int64
	local, fallback   atomic.Uint64
	policyPrimary     atomic.Uint64
	bootstrapLSN      string
	bootstrapSystem   string
	bootstrapTimeline int
}

func (h *HandlerContext) WriteDB() *sqlx.DB {
	if h.PrimaryDB != nil {
		return h.PrimaryDB
	}
	return h.DB
}

// Data payloads may be eventually consistent. Authentication and current
// permissions are independently checked on Primary before returning a response.
func (h *HandlerContext) dataPool(c *echo.Context) (*sqlx.DB, bool) {
	r := h.Replication
	if c.Request().Method == http.MethodGet && (replicaDataRoute(c.Path()) || c.Request().URL.Path == "/api/me") && r != nil && time.Since(time.Unix(0, r.healthyAt.Load())) < 2*time.Second {
		return r.replica, true
	}
	return h.WriteDB(), false
}

func (h *HandlerContext) readUserPayload(c *echo.Context, id string) (User, error) {
	local, _ := c.Get("usingReplica").(bool)
	user, err := getUser(c.Request().Context(), h.ReadDB(c), id)
	if err != nil && local {
		h.Replication.healthyAt.Store(0)
		h.Replication.fallback.Add(1)
		if h.PrimaryAPI != nil {
			var payload primaryUserPayload
			if err := h.callPrimary(c, primaryRPCRequest{Op: "user_payload", ID: id}, &payload); err != nil {
				return User{}, err
			}
			payload.User.SecretKeyHash = payload.SecretKeyHash
			err := payload.User.setImageKey()
			return payload.User, err
		}
		return getUser(c.Request().Context(), h.WriteDB(), id)
	}
	return user, err
}

func (h *HandlerContext) ReadDB(c *echo.Context) readDB {
	if db, ok := c.Get("readDB").(readDB); ok {
		return db
	}

	return h.WriteDB()
}

func newReplicaRouter(config *Config, primary *sqlx.DB, clients ...*primaryAPIClient) (*replicaRouter, func()) {
	if config.ReplicaHost == "" {
		return nil, func() {}
	}
	if !((config.Region == "europe" && config.ReplicaName == "isu02") || (config.Region == "america" && config.ReplicaName == "isu03")) || (config.ReplicaHost != "127.0.0.1" && config.ReplicaHost != "localhost") {
		panic("invalid replica region/name")
	}
	replica, err := sqlx.Open("pgx", config.ReplicaDSN())
	if err != nil {
		panic(err)
	}
	replica.SetMaxOpenConns(config.DBMaxOpenConns)
	replica.SetMaxIdleConns(config.DBMaxOpenConns)
	replica.SetConnMaxLifetime(3 * time.Minute)
	r := &replicaRouter{primary: primary, replica: replica, name: config.ReplicaName, primaryHost: config.DBHost}
	if len(clients) > 0 {
		r.api = clients[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			check, stop := context.WithTimeout(ctx, 900*time.Millisecond)
			err := r.check(check)
			stop()
			if err != nil {
				if r.healthyAt.Swap(0) != 0 {
					log.Printf("replica disabled: %v", err)
				}
			} else {
				if r.healthyAt.Swap(time.Now().UnixNano()) == 0 {
					log.Printf("replica ready name=%s", r.name)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if time.Now().Unix()%10 == 0 {
					log.Printf("replica reads name=%s local=%d fallback=%d policy_primary=%d healthy=%t", r.name, r.local.Load(), r.fallback.Load(), r.policyPrimary.Load(), r.healthyAt.Load() != 0)
				}
			}
		}
	}()
	return r, func() {
		cancel()
		<-done
		replica.Close()
		log.Printf("replica reads local=%d fallback=%d", r.local.Load(), r.fallback.Load())
	}
}

// This is exclusively a background query. No HTTP read asks Primary for an LSN.
func (r *replicaRouter) check(ctx context.Context) error {
	var identity primaryIdentity
	var err error
	if r.api != nil {
		err = r.api.call(ctx, "", "", primaryRPCRequest{Op: "replica_health", ReplicaName: r.name}, &identity)
	} else {
		identity, err = primaryReplicaIdentity(ctx, r.primary, r.name)
	}
	if err != nil {
		return err
	}
	if !identity.Safe {
		return fmt.Errorf("expected Primary/physical sender unavailable")
	}
	target := "0/0"
	if r.healthyAt.Load() == 0 {
		if r.bootstrapLSN == "" || r.bootstrapSystem != identity.System || r.bootstrapTimeline != identity.Timeline {
			r.bootstrapLSN = identity.LSN
			r.bootstrapSystem = identity.System
			r.bootstrapTimeline = identity.Timeline
		}
		target = r.bootstrapLSN
	}
	var safe bool
	err = r.replica.GetContext(ctx, &safe, `SELECT pg_is_in_recovery() AND NOT pg_is_wal_replay_paused()
 AND s.system_identifier::text=$1 AND c.timeline_id=$2
 AND pg_last_wal_replay_lsn()>=$3::pg_lsn
 AND EXISTS(SELECT FROM pg_stat_wal_receiver WHERE status='streaming' AND sender_host=$4 AND received_tli=$2 AND slot_name=$5)
 FROM pg_control_system() s CROSS JOIN pg_control_checkpoint() c`, identity.System, identity.Timeline, target, r.primaryHost, r.name)
	if err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("replica identity/replay/receiver not ready")
	}
	r.bootstrapLSN = ""
	return nil
}

func replicaDataRoute(path string) bool {
	switch path {
	case "/api/me", "/api/feeds/:feed", "/api/posts/:postId", "/api/posts/me/current", "/api/memories", "/api/memories/:date", "/api/me/export", "/api/users", "/api/users/:userId", "/api/posts/:postId/comments", "/api/posts/:postId/realmojis":
		return true
	}
	return false
}

// Primary/Asia keeps its direct pool. Overseas payload SELECTs use local DB;
// authentication and the request permission snapshot always use Primary.
func ReadRoutingMiddleware(h *HandlerContext) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			r := h.Replication
			if r == nil {
				return next(c)
			}
			if c.Request().Method != http.MethodGet || !replicaDataRoute(c.Path()) {
				if c.Request().Method == http.MethodGet {
					r.policyPrimary.Add(1)
				}
				return next(c)
			}
			pool, local := h.dataPool(c)
			if h.PrimaryAPI != nil && !local {
				return h.PrimaryAPI.forward(c)
			}
			c.Set("readDB", pool)
			c.Set("usingReplica", local)
			req := c.Request()
			c.SetRequest(req.WithContext(context.WithValue(req.Context(), primaryReadKey{}, &primaryReadScope{primary: h.WriteDB(), local: local, api: h.PrimaryAPI, auth: req.Header.Get("Authorization"), ip: c.RealIP()})))
			defer func() { c.Set("readDB", nil); c.Set("usingReplica", nil) }()
			err := next(c)
			var apiError *APIError
			response, _ := echo.UnwrapResponse(c.Response())
			if err != nil && local && !errors.As(err, &apiError) && response != nil && !response.Committed {
				r.healthyAt.Store(0)
				r.fallback.Add(1)
				if h.PrimaryAPI != nil {
					return h.PrimaryAPI.forward(c)
				}
				c.Set("readDB", h.WriteDB())
				c.Set("usingReplica", false)
				if scope := primaryScope(c.Request().Context()); scope != nil {
					scope.local = false
				}
				err = next(c)
			}
			if GetSession(c) != nil {
				if local {
					r.local.Add(1)
				} else {
					r.fallback.Add(1)
				}
			}
			return err
		}
	}
}

// pg_stat_replication replay_lsn is the standby's replay acknowledgement, not
// merely sent_lsn/flush_lsn. Both named slots must belong to the expected peers.
func waitForReplicas(ctx context.Context, primary *sqlx.DB) error {
	started := time.Now()
	defer func() { log.Printf("initialize replica_wait=%s", time.Since(started)) }()
	var target struct {
		LSN      string `db:"lsn"`
		Timeline int    `db:"timeline"`
		System   string `db:"system"`
	}
	if err := primary.GetContext(ctx, &target, `SELECT pg_current_wal_flush_lsn()::text AS lsn,c.timeline_id AS timeline,s.system_identifier::text AS system FROM pg_control_checkpoint() c CROSS JOIN pg_control_system() s`); err != nil {
		return err
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		var ready bool
		err := primary.GetContext(ctx, &ready, `SELECT NOT pg_is_in_recovery()
 AND (SELECT timeline_id=$2 FROM pg_control_checkpoint()) AND (SELECT system_identifier::text=$3 FROM pg_control_system())
 AND (SELECT count(*)=2 FROM pg_stat_replication r JOIN pg_replication_slots s ON s.active_pid=r.pid
 WHERE (r.application_name,r.client_addr::text,s.slot_name) IN
 (('isu02','192.168.2.10/32','isu02'),('isu03','192.168.3.10/32','isu03'))
 AND s.slot_type='physical' AND r.state='streaming' AND r.replay_lsn >= $1::pg_lsn)`, target.LSN, target.Timeline, target.System)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("replica initialize barrier: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// A deadline bounds synchronous COMMIT too. A timeout is an unknown outcome:
// handlers return an error and must never automatically retry the mutation.
func WriteDeadlineMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		req := c.Request()
		if req.Method == http.MethodGet || req.Method == http.MethodHead || c.Path() == "/api/initialize" || c.Path() == "/internal/image-files/:imageId" {
			return next(c)
		}
		ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
		defer cancel()
		c.SetRequest(req.WithContext(ctx))
		err := next(c)
		if ctx.Err() != nil {
			log.Printf("write deadline: outcome may be committed; do not retry automatically path=%s", c.Path())
			return ctx.Err()
		}
		return err
	}
}
