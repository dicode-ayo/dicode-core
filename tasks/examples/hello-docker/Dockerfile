FROM alpine:3.21 AS build

RUN apk add --no-cache curl

COPY greet.sh /greet.sh
RUN chmod +x /greet.sh

ENTRYPOINT ["/greet.sh"]

# dicode task test builds and runs this stage. Built separately rather than
# FROM build: it only checks greet.sh's own output, which needs no package
# install to build or run, unlike the build stage's apk add.
FROM alpine:3.21 AS test
COPY greet.sh /greet.sh
RUN chmod +x /greet.sh
CMD ["sh", "-c", "/greet.sh | grep -q 'Hello from Docker'"]
