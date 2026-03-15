# TypeScript build
FROM node:25-alpine AS ts
WORKDIR /src
COPY package.json package-lock.json tsconfig.json ./
COPY src/ src/
RUN npm ci && npx tsc

# Go build
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
COPY --from=ts /src/static/js/ static/js/
RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -o /bin/skua-site ./cmd/server

# Runtime
FROM alpine:3.23
RUN apk --no-cache add ca-certificates
COPY --from=build /bin/skua-site /usr/local/bin/skua-site

ENV ADDR=:3000
# Required: GitHub PAT with repo read access (certifications is private)
# ENV GITHUB_TOKEN=
# Optional: shared secret for POST /api/docs/refresh
# ENV REFRESH_SECRET=
ENV PRESETS_DIR=/data/presets
VOLUME /data/presets

EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget -qO- http://localhost:3000/health || exit 1
ENTRYPOINT ["skua-site"]
