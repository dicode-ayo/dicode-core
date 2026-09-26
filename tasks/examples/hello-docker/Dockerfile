# Built and run by dicode task test (--target test). A plain build (no
# --target, what production uses) defaults to the file's last stage, so
# this stage must not be last. curl isn't installed here, so greet.sh's
# curl-version line reports "not found" in the output — only the greeting
# text below is checked.
FROM alpine:3.21 AS test
COPY greet.sh /greet.sh
RUN chmod +x /greet.sh
CMD ["sh", "-c", "/greet.sh | grep -q 'Hello from Docker'"]

FROM alpine:3.21 AS build

RUN apk add --no-cache curl

COPY greet.sh /greet.sh
RUN chmod +x /greet.sh

ENTRYPOINT ["/greet.sh"]
