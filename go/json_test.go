package isureal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
)

func TestGoJSONSerializer(t *testing.T) {
	e := echo.New()
	e.JSONSerializer = goJSONSerializer{}
	e.POST("/", func(c *echo.Context) error {
		var input struct {
			Name string `json:"name"`
		}
		if err := c.Bind(&input); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, input)
	})
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"name":"日本語<&>"}`, http.StatusOK},
		{`{"name":`, http.StatusBadRequest},
		{`{"name":123}`, http.StatusBadRequest},
		{`{"name":"a"} {"name":"b"}`, http.StatusBadRequest},
	} {
		t.Run(tc.body, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status == http.StatusOK && rec.Body.String() != "{\"name\":\"日本語\\u003c\\u0026\\u003e\"}\n" {
				t.Fatalf("unexpected JSON response: %s", rec.Body.String())
			}
		})
	}
}
