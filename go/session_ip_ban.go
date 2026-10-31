package isureal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
)

// Combine two Primary checks for overseas authenticated routes. Asia retains
// its original cached-ban + session path. There is no cross-request auth cache.
func combineSessionIPBan(h *HandlerContext, c *echo.Context) bool {
	if h.Config == nil || (h.Config.Region != "europe" && h.Config.Region != "america") || !strings.HasPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ") {
		return false
	}
	switch c.Path() {
	case "/api/auth/logout", "/api/users", "/api/users/:userId", "/api/me/export", "/api/me",
		"/api/me/profile-image", "/api/me/account-deletion", "/api/posts", "/api/posting-window/current",
		"/api/posts/me/current", "/api/posts/:postId", "/api/feeds/:feed", "/api/memories", "/api/memories/:date",
		"/api/posts/:postId/comments", "/api/comments/:commentId", "/api/posts/:postId/realmojis", "/api/realmojis/:realMojiId",
		"/api/notifications", "/api/friend-requests", "/api/friend-requests/:requestId/:action", "/api/relationships/:kind",
		"/api/friendships/:userId", "/api/follows/:userId", "/api/blocks/:userId":
		return true
	}
	return false
}

func checkPrimarySessionAndBan(h *HandlerContext, c *echo.Context) error {
	token := strings.TrimPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
	hash := sha256.Sum256([]byte(token))
	var row struct {
		Banned    bool           `db:"banned"`
		UserID    sql.NullString `db:"user_id"`
		TokenHash sql.NullString `db:"token_hash"`
		CreatedAt sql.NullTime   `db:"created_at"`
		Region    sql.NullString `db:"region"`
	}
	err := h.WriteDB().GetContext(c.Request().Context(), &row, `SELECT
 EXISTS(SELECT FROM ip_bans WHERE ip=$2 AND banned_until>clock_timestamp()) AS banned,
 current_session.user_id,current_session.token_hash,current_session.created_at,current_session.region
 FROM (SELECT 1) AS request
 LEFT JOIN LATERAL (
  SELECT s.user_id,s.token_hash,s.created_at,u.region
  FROM login_sessions s JOIN users u ON u.id=s.user_id
  WHERE s.token_hash=$1 AND s.active=true AND u.status='active'
  LIMIT 1
 ) AS current_session ON true`, hex.EncodeToString(hash[:]), c.RealIP())
	if err != nil {
		return err
	}
	if row.Banned {
		return NewAPIError(http.StatusForbidden, "ip_banned")
	}
	var session *Session
	if row.UserID.Valid {
		session = &Session{UserID: row.UserID.String, TokenHash: row.TokenHash.String, CreatedAt: row.CreatedAt.Time, Region: row.Region.String}
	}
	c.Set("session", session)
	c.Set("primary_session_checked", true)
	return nil
}
