# syntax=docker/dockerfile:1

FROM justb4/jmeter

RUN apk --no-cache add ca-certificates git

WORKDIR /root/

ENV ENTRYPOINT_CMD="/entrypoint.sh"

COPY dist/runner /bin/runner
ADD plugins/ ${JMETER_CUSTOM_PLUGINS_FOLDER}
ADD lib/ ${JMETER_HOME}/lib/

ENTRYPOINT ["/bin/runner"]
