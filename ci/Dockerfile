ARG DB_IMAGE

FROM ${DB_IMAGE}

RUN apt-get update && apt-get install --no-install-recommends -y \
    netcat-openbsd ca-certificates curl iputils-ping dnsutils

# RUN sh -c 'echo "deb http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list' && \
#     curl -fSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /etc/apt/trusted.gpg.d/pgdg.asc && \
#     apt install postgresql-client-13 postgresql-client-14 postgresql-client-15 -y

COPY dbq /usr/local/bin/dbq

EXPOSE 8080

# An empty entrypoint with bash as the default command keeps the image usable as
# a database client shell (`docker run <img>`) while still allowing the server to
# be started directly (`docker run <img> dbq server`).
ENTRYPOINT []
CMD ["/bin/bash"]
