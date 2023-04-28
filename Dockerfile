FROM ubuntu:22.04

ARG TARGETARCH

WORKDIR /app

RUN apt-get update && \
    apt-get install -y \
        curl \
        unzip \
        && \
    \
    $(if [ "$TARGETARCH" = "amd64" ]; then \
        curl -sSL "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip"; \
    elif [ "$TARGETARCH" = "arm64" ]; then \
        curl -sSL "https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip" -o "awscliv2.zip"; \
    else \
        echo "Invalid target arch in $TARGETARCH" >&2; \
        exit 1; \
    fi) && \
    unzip awscliv2.zip && \
    ./aws/install && \
    rm -rf aws && \
    rm awscliv2.zip && \
    \
    apt-get -y autoremove && \
    apt-get -y clean && \
    rm -rf /var/lib/apt/lists/* && \
    rm -rf /tmp/* && \
    rm -rf /var/tmp/*

COPY update-route53.sh /app

CMD ["/bin/bash", "-c", "/app/update-route53.sh"]

