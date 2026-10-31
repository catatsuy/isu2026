package isureal

import (
	"io"
	"mime/multipart"
)

// The multipart parser has already counted the exact part size. Allocate once
// instead of repeatedly growing and copying an io.ReadAll buffer.
func readImagePart(file multipart.File, header *multipart.FileHeader) ([]byte, error) {
	data := make([]byte, header.Size)
	_, err := io.ReadFull(file, data)
	return data, err
}
