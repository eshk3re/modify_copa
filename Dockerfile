FROM golang:alpine AS builder

RUN apk add --no-cache git make

WORKDIR /build

RUN git clone https://github.com/project-copacetic/copacetic.git . && rm ./pkg/patch/cmd.go ./pkg/patch/patch.go

COPY patch.go cmd.go ./pkg/patch/

RUN make && mv dist/linux_amd64/release/copa /copa

FROM alpine:latest

RUN apk add --no-cache bash curl docker skopeo \
    && curl -sSL https://github.com/aquasecurity/trivy/releases/download/v0.57.1/trivy_0.57.1_Linux-64bit.tar.gz \
    | tar -xz -C /usr/local/bin \
    && chmod +x /usr/local/bin/trivy \
    && rm -rf /var/cache/apk/*

RUN addgroup -S copgroup && adduser -S copuser -G copgroup

COPY --from=builder /copa /usr/local/bin/copa

RUN rm -rf /var/cache/apk/*

WORKDIR /workspace

RUN chown -R copuser:copgroup /workspace

USER copuser

CMD ["sh"]
