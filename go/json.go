package isureal

import (
	"bytes"
	"sync"

	"github.com/goccy/go-json"
	"github.com/labstack/echo/v5"
)

type goJSONSerializer struct{}

var jsonBufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func (goJSONSerializer) Serialize(c *echo.Context, target any, indent string) error {
	enc := json.NewEncoder(c.Response())
	if indent != "" {
		enc.SetIndent("", indent)
	}
	return enc.Encode(target)
}

func (goJSONSerializer) Deserialize(c *echo.Context, target any) error {
	buf := jsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		// Avoid retaining large request bodies in the pool.
		if buf.Cap() <= 1<<16 {
			jsonBufferPool.Put(buf)
		}
	}()
	if _, err := buf.ReadFrom(c.Request().Body); err != nil {
		return echo.ErrBadRequest.Wrap(err)
	}
	if err := json.Unmarshal(buf.Bytes(), target); err != nil {
		return echo.ErrBadRequest.Wrap(err)
	}
	return nil
}
