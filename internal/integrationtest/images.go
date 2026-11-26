package integrationtest

// Public ECR mirrors of the Docker Hub library images the integration suites
// use. The suites live in two packages (container and container_test), so the
// refs live here for the same reason the preflight does: neither test package
// can own a value the other one needs.
//
// .github/workflows/ci.yml caches exactly these refs before running the Docker
// integration suite. workflowcheck keeps that list in sync with this one.
const (
	RedisImage  = "public.ecr.aws/docker/library/redis:7-alpine"
	NginxImage  = "public.ecr.aws/docker/library/nginx:alpine"
	AlpineImage = "public.ecr.aws/docker/library/alpine:latest"
)
