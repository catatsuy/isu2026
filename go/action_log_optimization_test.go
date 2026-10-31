package isureal

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
)

func TestActionLogErrorDefault(t *testing.T) {
	t.Setenv("ISUREAL_LOG_LEVEL", "")
	if ConfigFromEnv().LogLevel != "error" {
		t.Fatal("default log level is not error")
	}
	for _, fail := range []bool{false, true} {
		var output bytes.Buffer
		e := echo.New()
		e.HTTPErrorHandler = APIErrorHandler
		e.Use(ActionLogMiddleware("error", &output))
		e.GET("/api/me", func(c *echo.Context) error {
			if fail {
				return errors.New("diagnostic failure")
			}
			return c.NoContent(http.StatusOK)
		})
		response := httptest.NewRecorder()
		e.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/me", nil))
		if !fail {
			if response.Code != 200 || output.Len() != 0 {
				t.Fatal("successful request logged or response changed")
			}
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if response.Code != 500 || entry["error"] != "diagnostic failure" || entry["status"] != float64(500) {
			t.Fatalf("missing failure diagnostics: %s", output.Bytes())
		}
	}
}
