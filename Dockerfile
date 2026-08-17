# The release pipeline builds the runtime images natively via Dagger (see
# runtimeImages in ci/build.go); this file is the equivalent recipe for a local
# `docker build` against a dist/ binary.
#
# The binary is statically linked (CGO_ENABLED=0, -buildmode=exe), so scratch
# carries it. Root certificates are the one thing it cannot supply itself:
# every API call is HTTPS, and with no system trust store Go rejects each one
# as signed by an unknown authority.
FROM public.ecr.aws/docker/library/alpine:3.22 AS certs

FROM scratch

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY hcp_archiver /usr/local/bin/

ENTRYPOINT ["hcp_archiver"]
