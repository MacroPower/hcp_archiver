FROM scratch

COPY tfc_archiver /usr/local/bin/

ENTRYPOINT ["tfc_archiver"]
