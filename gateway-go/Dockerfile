FROM alpine:3.20
RUN apk add --no-cache curl ca-certificates
COPY gateway /gateway
COPY static/ /static/
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=10s --retries=3 --start-period=10s \
  CMD curl -sf http://localhost:8000/health
CMD ["/gateway"]
