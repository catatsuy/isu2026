package isureal

import (
	"database/sql"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func putProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if c.Request().Header.Get("Content-Type") != "image/jpeg" {
			return NewAPIError(http.StatusUnsupportedMediaType, "unsupported_media_type")
		}
		data, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err
		}
		if err := decodeUploadJPEG(c, data, 512, "invalid_profile_image"); err != nil {
			return err
		}

		ctx := c.Request().Context()
		// Persist immutable bytes and durable replication intent before taking DB locks.
		// Failed transactions may leave unreferenced files, which are never public.
		imageID := uuid.NewString()
		if err := h.ImageFiles.Save(ctx, imageID, data); err != nil {
			return err
		}
		return h.finishImageUpload(c, primaryImageUpload{Kind: "profile", ImageID: imageID})
	}
}

func (h *HandlerContext) commitProfileImage(c *echo.Context, imageID string) error {
	ctx := c.Request().Context()
	userID := GetSession(c).UserID

	tx, err := h.WriteDB().BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldID *string
	if err := tx.GetContext(ctx, &oldID, `SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		return err
	}
	if err := recheckUploadSession(c, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO image_objects(id,owner_user_id,data,media_type) VALUES ($1,$2,$3,'image/jpeg')`, imageID, userID, nil); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET profile_image_id=$2,updated_at=clock_timestamp() WHERE id=$1`, userID, imageID); err != nil {
		return err
	}
	if oldID != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM image_objects WHERE id=$1`, *oldID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func deleteProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.WriteDB().BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var imageID *string
		err = tx.GetContext(ctx, &imageID, `SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		if imageID == nil {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET profile_image_id=NULL,updated_at=clock_timestamp() WHERE id=$1`, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM image_objects WHERE id=$1`, *imageID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
