package cli

import (
	"context"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/service"
)

type managementClient interface {
	Request(context.Context, service.ManagementRequest) (service.ManagementReply, error)
	Close()
}
type managementFactory func(context.Context, config.Root) (managementClient, error)
type nativeClient struct{ *service.Controller }

func (nativeClient) Close() {}
func nativeManagement(build Build) managementFactory {
	return func(ctx context.Context, root config.Root) (managementClient, error) {
		c, err := serviceController(root.Path, build)
		if err != nil {
			return nil, err
		}
		boot, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()
		if _, err = c.EnsureManagement(boot); err != nil {
			return nil, err
		}
		return nativeClient{c}, nil
	}
}
