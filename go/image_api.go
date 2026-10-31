package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"
)

type getImageRequest struct {
	ImageID string `param:"imageId" json:"-"`
	Exp     string `query:"exp" json:"-"`
	Sig     string `query:"sig" json:"-"`
}

func getImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		var input getImageRequest
		if err := c.Bind(&input); err != nil {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		query, err := url.ParseQuery(c.Request().URL.RawQuery)
		if err != nil || len(query["exp"]) != 1 || len(query["sig"]) != 1 {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		exp, err := strconv.ParseInt(input.Exp, 10, 64)
		if err != nil || strconv.FormatInt(exp, 10) != input.Exp || exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		sig, err := base64.RawURLEncoding.Strict().DecodeString(input.Sig)
		if err != nil || len(sig) != sha256.Size || base64.RawURLEncoding.EncodeToString(sig) != input.Sig {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}

		ctx := c.Request().Context()
		checkedAt := time.Now()
		object, err := h.imageAuthorization(c, input.ImageID, false)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if err != nil {
			return err
		}
		key, err := imageSigningKey(object.SecretKeyHash)
		if err != nil {
			return err
		}
		if !hmac.Equal(sig, imageSignature(key, input.ImageID, exp)) {
			c.Set("abuse", "invalid_signature")
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}

		if !object.Visible {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		uri, err := h.ImageFiles.Ensure(ctx, input.ImageID)
		if err != nil {
			return err
		}

		// A slow replica fetch must not extend the deletion grace period.
		if time.Since(checkedAt) >= time.Second {
			if object, err = h.imageAuthorization(c, input.ImageID, true); errors.Is(err, sql.ErrNoRows) {
				return NewAPIError(http.StatusForbidden, "image_forbidden")
			} else if err != nil {
				return err
			}
			if !object.Visible {
				return NewAPIError(http.StatusForbidden, "image_forbidden")
			}
		}

		if exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		c.Response().Header().Set("Content-Type", object.MediaType)
		c.Response().Header().Set("X-Accel-Redirect", uri)
		return c.NoContent(http.StatusOK)
	}
}

// Read the owner once. CASE short-circuits profile/post images before the
// RealMoji lookup. Owner predicates also allow searches through owner indexes;
// all deletion and active-account checks remain part of the same snapshot.
const imageAccessQuery = `
 SELECT i.media_type, u.secret_key_hash,
   CASE
     WHEN u.profile_image_id = i.id THEN true
     WHEN EXISTS (
       SELECT 1 FROM posts p
       WHERE p.author_id = u.id AND p.image_id = i.id AND p.deleted_at IS NULL
     ) THEN true
     ELSE EXISTS (
       SELECT 1 FROM realmojis r
       JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
       JOIN users author ON author.id = p.author_id AND author.status = 'active'
       WHERE r.reactor_id = u.id AND r.image_id = i.id
         AND r.replaced_at IS NULL AND r.deleted_at IS NULL
     )
   END AS visible
 FROM image_objects i
 JOIN users u ON u.id = i.owner_user_id AND u.status = 'active'
 WHERE i.id = $1
`
