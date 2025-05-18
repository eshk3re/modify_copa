FROM golang:alpine3.21 AS builder

RUN apk add --no-cache git make

WORKDIR /build

RUN wget https://github.com/project-copacetic/copacetic/archive/refs/tags/v0.10.0.tar.gz && \
    tar -xvf v0.10.0.tar.gz && cd copacetic-0.10.0 && rm ./pkg/patch/cmd.go ./pkg/patch/patch.go

COPY patch.go cmd.go ./copacetic-0.10.0/pkg/patch/

RUN cd copacetic-0.10.0 && make && mv dist/linux_amd64/release/copa /copa

FROM alpine:3.21

RUN apk add --no-cache bash curl docker skopeo \
    && curl -sSL https://github.com/aquasecurity/trivy/releases/download/v0.57.1/trivy_0.57.1_Linux-64bit.tar.gz \
    | tar -xz -C /usr/local/bin \
    && chmod +x /usr/local/bin/trivy \
    && rm -rf /var/cache/apk/* && apk update && apk upgrade

RUN addgroup -S copgroup && adduser -S copuser -G copgroup

COPY --from=builder /copa /usr/local/bin/copa

RUN rm -rf /var/cache/apk/*

WORKDIR /workspace

RUN chown -R copuser:copgroup /workspace

USER copuser

CMD ["sh"]
