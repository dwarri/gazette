VERSION=1.0.0

all: build

.PHONY: build
build:
	@docker build -f build/Dockerfile.gazette-build -t gazette-build .
	@docker build -f build/cmd/Dockerfile.gazette -t gazette .

push:
	docker tag gazette-build gcr.io/liveramp-eng/gazette-build-azim:$(VERSION)
	docker tag gazette gcr.io/liveramp-eng/gazette-azim:$(VERSION)
	docker push gcr.io/liveramp-eng/gazette-build-azim:$(VERSION)
	docker push gcr.io/liveramp-eng/gazette-azim:$(VERSION)