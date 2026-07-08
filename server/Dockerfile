# The claude_spawner server, containerized. With SSH-native execution
# (SPAWNER_SSH=1) the container drives the host over SSH — claude runs on the host,
# not in this image — so this is deliberately lean: just the static binary. There is
# no host broker; the container reaches the host's sshd like any other SSH target.
#
# Run it with host networking (so localhost:22 is the host sshd) and the user's home
# mounted read-only at the same path (so the server browses/reads transcripts where
# claude writes them on the host). See deploy/ for the compose/run recipe.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -trimpath -o /spawner ./

FROM alpine:3.20
# ca-certificates for any outbound TLS. openssh-client is NOT needed for turns (claude
# SSH is Go-native) — it's here only so the app's restart button can SSH to the host
# and launch the detached container rebuild (deploy/rebuild-container.sh), which must
# run on the host because `compose up --build` recreates this very container.
RUN apk add --no-cache ca-certificates openssh-client
COPY --from=build /spawner /usr/local/bin/spawner
ENTRYPOINT ["/usr/local/bin/spawner"]
