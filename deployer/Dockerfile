FROM registry.access.redhat.com/ubi9/ubi:latest

# Install required tools
RUN yum install -y --allowerasing \
    git \
    curl \
    bash \
    tar \
    && yum clean all

# Install OpenShift CLI (oc) - using specific version 4.14
RUN curl -L https://mirror.openshift.com/pub/openshift-v4/clients/ocp/4.14.0/openshift-client-linux.tar.gz \
    -o /tmp/oc.tar.gz && \
    tar -xzf /tmp/oc.tar.gz -C /usr/local/bin oc kubectl && \
    rm /tmp/oc.tar.gz && \
    chmod +x /usr/local/bin/oc /usr/local/bin/kubectl

# Verify only git - skip oc/kubectl verification as it needs actual runtime
RUN git --version

# Set working directory
WORKDIR /workspace

CMD ["/bin/bash"]
