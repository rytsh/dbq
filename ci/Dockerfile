ARG DB_IMAGE

FROM ${DB_IMAGE}

RUN apt-get update && apt-get install --no-install-recommends -y \
    netcat-openbsd ca-certificates curl iputils-ping dnsutils

# RUN sh -c 'echo "deb http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list' && \
#     curl -fSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /etc/apt/trusted.gpg.d/pgdg.asc && \
#     apt install postgresql-client-13 postgresql-client-14 postgresql-client-15 -y

COPY dbq /usr/local/bin/dbq

ENV DBQ_SERVER_HOST=0.0.0.0

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/dbq"]
CMD ["server"]
