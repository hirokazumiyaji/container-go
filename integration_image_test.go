//go:build integration

package container

// Public ECR mirrors of Docker Hub library images. Integration tests
// pull through these refs to avoid anonymous Docker Hub rate limits.
const (
	integrationRedis  = "public.ecr.aws/docker/library/redis:7-alpine"
	integrationNginx  = "public.ecr.aws/docker/library/nginx:alpine"
	integrationAlpine = "public.ecr.aws/docker/library/alpine:latest"
)
