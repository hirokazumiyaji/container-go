//go:build integration

package container

import "github.com/hirokazumiyaji/container-go/internal/integrationtest"

// Public ECR mirrors of Docker Hub library images. The refs are defined once
// in integrationtest, which the external test package imports too, so a
// mirror or tag change cannot leave the two suites on different images.
const (
	integrationRedis  = integrationtest.RedisImage
	integrationNginx  = integrationtest.NginxImage
	integrationAlpine = integrationtest.AlpineImage
)
