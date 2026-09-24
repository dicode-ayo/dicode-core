FROM alpine:3.21 AS build

RUN apk add --no-cache curl

COPY greet.sh /greet.sh
RUN chmod +x /greet.sh

ENTRYPOINT ["/greet.sh"]

# dicode task test builds and runs this stage. Built separately rather than
# FROM build, so it needs no package install: curl isn't installed here, so
# greet.sh's curl-version line reports "not found" in the captured output —
# expected, and only the greeting line below is asserted on.
FROM alpine:3.21 AS test
COPY greet.sh /greet.sh
RUN chmod +x /greet.sh
CMD ["sh", "-c", "/greet.sh | grep -q 'Hello from Docker'"]
