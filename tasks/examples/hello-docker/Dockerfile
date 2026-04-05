FROM alpine:3.21

RUN apk add --no-cache curl

COPY greet.sh /greet.sh
RUN chmod +x /greet.sh

ENTRYPOINT ["/greet.sh"]
