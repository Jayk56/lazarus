FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818

ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install --no-install-recommends --yes openssh-server python3 \
    && useradd --create-home --shell /bin/sh awx-fixture \
    && passwd --delete awx-fixture \
    && install -d -m 0755 /var/lib/awx-fixture /run/sshd \
    && chown awx-fixture:awx-fixture /var/lib/awx-fixture \
    && find /var/lib/apt/lists -mindepth 1 -delete

COPY fixture-entrypoint.sh /usr/local/bin/fixture-entrypoint
COPY fixture-service.sh /usr/local/bin/fixture-service
COPY systemctl.sh /usr/local/bin/systemctl
COPY service.sh /usr/local/bin/service
COPY sshd_config /etc/ssh/sshd_config

RUN chmod 0755 \
      /usr/local/bin/fixture-entrypoint \
      /usr/local/bin/fixture-service \
      /usr/local/bin/systemctl \
      /usr/local/bin/service \
    && chmod 0644 /etc/ssh/sshd_config \
    && ln -sf /usr/local/bin/systemctl /usr/bin/systemctl \
    && ln -sf /usr/local/bin/service /usr/sbin/service

EXPOSE 22

ENTRYPOINT ["/usr/local/bin/fixture-entrypoint"]
