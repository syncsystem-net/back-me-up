package provider

import (
	"context"
	"io"
)

type Provider interface {
	Name() string
	Login(ctx context.Context, email, password string) error
	Upload(ctx context.Context, reader io.Reader, remotePath string, size int64) error
	Download(ctx context.Context, remotePath string, writer io.Writer) error
	Delete(ctx context.Context, remotePath string) error
	GetQuota(ctx context.Context) (totalBytes, usedBytes int64, err error)
}
