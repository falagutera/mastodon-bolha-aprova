# --- build -------------------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum VERSION ./
RUN go mod download
COPY . .
ARG BUILD=dev
# Leave modules out with tags, e.g. --build-arg TAGS="no_gitlab".
ARG TAGS=""
RUN CGO_ENABLED=0 go build -trimpath -tags "${TAGS}" -ldflags="-s -w -X main.build=${BUILD}" -o /aprova . \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.build=${BUILD}" -o /aprova-agent ./cmd/aprova-agent

# --- runtime -----------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /aprova /aprova
# The agent runs inside an app's own image: copy it out with -install (see README).
COPY --from=build /aprova-agent /aprova-agent
# distroless :nonroot's user, by number so the runtime can check it isn't root.
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --retries=3 CMD ["/aprova", "-healthcheck"]
ENTRYPOINT ["/aprova"]
