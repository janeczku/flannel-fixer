FROM golang:alpine AS build-env
ADD . /src
RUN cd /src && go build -o flannel-ip-setter

FROM alpine as final
RUN apk add --no-cache ca-certificates
COPY --from=build-env /src/flannel-ip-setter /bin/flannel-ip-setter

USER nobody

