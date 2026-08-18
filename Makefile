.PHONY: test vet integration

test:
	go test ./...

vet:
	go vet ./...

integration:
	go test -tags integration -count=1 -timeout 20m ./...
