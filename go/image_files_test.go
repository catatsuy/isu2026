package isureal

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func TestImageFileReplicas(t *testing.T) {
	ctx := context.Background()
	token := bytes.Repeat([]byte("k"), 32)
	replica := &ImageFileStore{Root: t.TempDir(), token: token}
	e := echo.New()
	e.PUT("/internal/image-files/:imageId", replica.peerHandler())
	e.GET("/internal/image-files/:imageId", replica.peerHandler())
	server := httptest.NewServer(e)
	defer server.Close()
	primary := &ImageFileStore{Root: t.TempDir(), Peers: []string{server.URL}, token: token, client: &http.Client{Timeout: time.Second}}
	id := "109d53fc-c136-4f3d-a150-f28046faebad"
	data := []byte("immutable jpeg")
	if err := primary.Save(ctx, id, data); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(ImageFilePath(replica.Root, id))
	if err != nil || !bytes.Equal(copied, data) {
		t.Fatalf("replica differs: %v", err)
	}
	if err := os.Remove(ImageFilePath(primary.Root, id)); err != nil {
		t.Fatal(err)
	}
	recovered, err := primary.Read(ctx, id)
	if err != nil || !bytes.Equal(recovered, data) {
		t.Fatalf("file recovery failed: %v", err)
	}
	if err := primary.Save(ctx, id, []byte("different")); err == nil {
		t.Fatal("immutable image overwritten")
	}
	request := httptest.NewRequest(http.MethodGet, "/internal/image-files/"+id, nil)
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal("unauthenticated internal download allowed")
	}
	primary.token = bytes.Repeat([]byte("x"), 32)
	if err := primary.Save(ctx, "209d53fc-c136-4f3d-a150-f28046faebad", data); err == nil {
		t.Fatal("unacknowledged replication accepted")
	}
}

func TestNewImageRequiresReplica(t *testing.T) {
	store := &ImageFileStore{Root: t.TempDir()}
	if err := store.Save(context.Background(), "id", []byte("image")); err == nil {
		t.Fatal("single local copy accepted")
	}
}
