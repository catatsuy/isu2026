package isureal

import (
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v5"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type searchUsersRequest struct {
	Q              string `query:"q"`
	BeforeUsername string `query:"beforeUsername"`
	BeforeID       string `query:"beforeId"`
}

type searchUsersResponse struct {
	Items   []relationshipUserResponse `json:"items"`
	HasMore bool                       `json:"hasMore"`
}

func normalizeSearchText(value string) string {
	ascii := true
	for i := 0; i < len(value); i++ {
		if value[i] >= 128 {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(value)
	}
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(value)))
}

func searchUsersHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input searchUsersRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Q, 1, 100) {
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		if (input.BeforeUsername == "") != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		local, _ := c.Get("usingReplica").(bool)
		blocked := map[string]bool{}
		if !local {
			var err error
			blocked, err = getBlockedUserIDs(ctx, h.WriteDB(), viewer)
			if err != nil {
				return err
			}
		}

		query := normalizeSearchText(input.Q)
		items := make([]relationshipUserResponse, 0)
		// Grow batches for sparse matches while keeping memory and each SQL
		// result bounded. The cursor always advances over scanned users.
		batchSize := 256
		beforeUsername, beforeID := input.BeforeUsername, input.BeforeID
		for len(items) < 51 {
			var users []User
			if err := h.ReadDB(c).SelectContext(ctx, &users, `SELECT `+strings.Join(userColumns, ", ")+` FROM users WHERE status='active' AND id<>$1 AND ($2='' OR (username,id)>($2,$3)) ORDER BY username, id LIMIT $4`, viewer, beforeUsername, beforeID, batchSize); err != nil {
				return err
			}
			var permissionStates map[string]userPermissionState
			if local {
				ids := []string{}
				for _, user := range users {
					if strings.Contains(normalizeSearchText(user.Username), query) || strings.Contains(normalizeSearchText(user.DisplayName), query) {
						ids = append(ids, user.ID)
					}
				}
				if len(ids) > 0 {
					var err error
					permissionStates, err = h.userPermissions(c, ids)
					if err != nil {
						return err
					}
				}
			}

			for _, user := range users {
				if local {
					state, ok := permissionStates[user.ID]
					if !ok || state.Blocked {
						continue
					}
					user.AccountType = state.AccountType
				} else {
					if blocked[user.ID] {
						continue
					}
					if !strings.Contains(normalizeSearchText(user.Username), query) && !strings.Contains(normalizeSearchText(user.DisplayName), query) {
						continue
					}
				}

				if err := user.setImageKey(); err != nil {
					return err
				}
				if user.AccountType != "certified_public" {
					user.Bio = ""
				}
				items = append(items, newRelationshipUserResponse(user))
				if len(items) == 51 {
					break
				}
			}
			if len(users) < batchSize {
				break
			}
			last := users[len(users)-1]
			beforeUsername, beforeID = last.Username, last.ID
			batchSize = min(batchSize*2, 4096)
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		if local && h.PrimaryAPI != nil {
			exact := false
			for _, item := range items {
				if strings.EqualFold(item.Username, input.Q) {
					exact = true
					break
				}
			}
			if len(items) == 0 || (!exact && len(input.Q) >= 8 && usernamePattern.MatchString(input.Q)) {
				return h.PrimaryAPI.forward(c)
			}
		}
		return c.JSON(http.StatusOK, searchUsersResponse{Items: items, HasMore: hasMore})
	}
}

type getUserRequest struct {
	UserID string `param:"userId"`
}

type getUserResponse relationshipUserResponse

func getUserHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getUserRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		user, err := h.readUserPayload(c, input.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err != nil {
			return err
		}
		if local, _ := c.Get("usingReplica").(bool); local {
			states, err := h.userPermissions(c, []string{user.ID})
			if err != nil {
				return err
			}
			state, ok := states[user.ID]
			if !ok || state.Blocked {
				return NewAPIError(http.StatusNotFound, "relationship_not_found")
			}
			user.AccountType = state.AccountType
			if user.AccountType != "certified_public" && !state.Friend {
				user.Bio = ""
			}
		} else {
			blocked, err := isBlocked(ctx, h.WriteDB(), viewer, user.ID)
			if err != nil {
				return err
			}
			if blocked {
				return NewAPIError(http.StatusNotFound, "relationship_not_found")
			}
			friends, err := getFriendIDs(ctx, h.WriteDB(), viewer)
			if err != nil {
				return err
			}
			if user.AccountType != "certified_public" && !slices.Contains(friends, user.ID) {
				user.Bio = ""
			}
		}

		return c.JSON(http.StatusOK, getUserResponse(newRelationshipUserResponse(user)))
	}
}

func newRelationshipUserResponse(user User) relationshipUserResponse {
	return relationshipUserResponse{ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, ProfileImageURL: newGetMeResponse(user).ProfileImageURL, Bio: user.Bio, AccountType: user.AccountType}
}
