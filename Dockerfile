FROM scratch

COPY hcp_archiver /usr/local/bin/

ENTRYPOINT ["hcp_archiver"]
