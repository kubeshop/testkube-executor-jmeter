# syntax=docker/dockerfile:1
FROM golang:1.19

WORKDIR /build

COPY . .

ENV CGO_ENABLED=0
ENV GOOS=linux
ENV ENTRYPOINT_CMD="/entrypoint.sh"

RUN cd cmd/agent;go build -o /runner -mod mod -a .

FROM justb4/jmeter:5.5

RUN apk --no-cache add ca-certificates git

WORKDIR /root/

COPY --from=0 /runner /bin/runner
ADD plugins/ ${JMETER_CUSTOM_PLUGINS_FOLDER}
ADD lib/ ${JMETER_HOME}/lib/

ENTRYPOINT ["/bin/runner"]
